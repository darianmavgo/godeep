package agx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"godeep/drivers/apple/asc"
	"godeep/drivers/apple/pmgr"
	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// Driver returns the GPU driver. win maps a physical window; win(0) is
// all of physical memory (page tables are reached by physical address).
//
// Stage 1 brings the GPU up to the point where the firmware and the
// kernel share the UAT: power, identity, coprocessor CPU, handoff, the
// kernel's page tables in slot 0. It then watches for MMU faults. RTKit
// and the firmware's init structure are stage 2 (docs/AGX.md).
func Driver(win func(base uint64) mmio.Shared) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		switch c := d.Config.(type) {
		case Config:
			return run(ctx, d, win, c, T6000)
		case error:
			return c
		}
		return errors.New("agx: no configuration from the board")
	}}
}

// GPU is the published service (lsdev gpu0).
type GPU struct {
	mu        sync.Mutex
	id        ID
	cfg       Config
	stage     string
	kernTTB   uint64 // TTB1: iBoot's table, firmware mapped
	lowerTTB  uint64 // TTB0: ours
	faults    int
	lastFault string
}

// Devices reports the GPU for lsdev.
func (g *GPU) Devices() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.cfg.Firmware
	out := []string{
		g.id.String(),
		fmt.Sprintf("firmware %d.%d.%d (compat %d.%d.%d); %s", f[0], f[1], f[2], g.cfg.Compat[0], g.cfg.Compat[1], g.cfg.Compat[2], g.stage),
		fmt.Sprintf("UAT: kernel TTB1 %#x, TTB0 %#x; %d MMU faults", g.kernTTB, g.lowerTTB, g.faults),
	}
	if g.lastFault != "" {
		out = append(out, "last fault: "+g.lastFault)
	}
	return out
}

// ID is the GPU's identity.
func (g *GPU) ID() ID { g.mu.Lock(); defer g.mu.Unlock(); return g.id }

func (g *GPU) setStage(s string) { g.mu.Lock(); g.stage = s; g.mu.Unlock() }

func run(ctx context.Context, d *dev.Device, win func(uint64) mmio.Shared, c Config, hw HwConfig) error {
	if d.DMA == nil {
		return errors.New("agx: the board has no DMA region")
	}
	for _, p := range c.Power {
		if _, err := pmgr.On(ctx, win(p.Addr)); err != nil {
			return fmt.Errorf("agx: power domain %s: %w", p.Name, err)
		}
	}
	sgx := win(c.SGX)
	id, err := ReadID(sgx)
	if err != nil {
		return err
	}
	if err := hw.Check(id); err != nil {
		return err
	}
	if !SupportedFirmware(c.Compat) {
		return fmt.Errorf("agx: %s: firmware compat %d.%d.%d has no known layout (12.3 or 13.5)",
			id, c.Compat[0], c.Compat[1], c.Compat[2])
	}
	g := &GPU{id: id, cfg: c, stage: "starting"}

	// As drm/asahi's probe: start the coprocessor so the firmware can
	// answer the handoff, then set up the UAT.
	asc.StartCPU(win(c.ASC))
	h := NewHandoff(win(c.Handoff.Base))
	if err := h.Init(ctx); err != nil {
		return fmt.Errorf("%s: %w", id, err)
	}
	phys := win(0)
	pages := &DMAPages{A: d.DMA}
	lower, err := NewPageTable(phys, pages, hw.OAS, UserBase, UserTop)
	if err != nil {
		return err
	}
	kern, err := BorrowPageTable(phys, pages, c.PageTables.Base, hw.OAS, KernBase, KernTop)
	if err != nil {
		return err
	}
	if err := NewTTBs(win(c.TTBs.Base)).SetKernel(ctx, h, lower.TTB(), kern.TTB()); err != nil {
		return err
	}
	g.kernTTB, g.lowerTTB = kern.TTB(), lower.TTB()
	g.stage = "UAT handed off; RTKit and firmware init: stage 2"
	d.Ready(g)

	// Report MMU faults (the firmware has the kernel VM; nothing of ours
	// is mapped yet, so any fault here is the firmware's).
	// The register holds a fault until it's cleared: count changes only.
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var last uint64
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			info := sgx.Read64(FAULT_INFO)
			if info == last {
				continue
			}
			last = info
			if f, ok := DecodeFault(info); ok {
				g.mu.Lock()
				g.faults++
				g.lastFault = f.String()
				g.mu.Unlock()
			}
		}
	}
}
