package agx

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"godeep/drivers/apple/agx/fw"
	"godeep/drivers/apple/asc"
	"godeep/drivers/apple/pmgr"
	"godeep/kernel/dev"
	"godeep/kernel/mmio"
	"godeep/kernel/rtkit"
)

// rtkitBootTimeout bounds the firmware's RTKit handshake.
const rtkitBootTimeout = 5 * time.Second

// compareBlob compares a structure we built with m1n1's calibration blob
// of it (what a known-good driver built), if there is one.
func compareBlob(name string, o *Object, size int, r Region, win func(uint64) mmio.Shared) string {
	if r.Size == 0 {
		return name + ": no calibration blob to compare"
	}
	m := win(r.Base)
	ours := o.Bytes()[:size]
	if r.Size != uint64(size) {
		return fmt.Sprintf("%s: %#x bytes, calibration blob %#x: different layout?", name, size, r.Size)
	}
	n := uint64(size) &^ 3
	for off := uint64(0); off < n; off += 4 {
		if m.Read32(off) != binary.LittleEndian.Uint32(ours[off:]) {
			return fmt.Sprintf("%s: differs from the calibration blob at %#x (blob %#x bytes)", name, off, r.Size)
		}
	}
	return fmt.Sprintf("%s: matches the calibration blob (%#x bytes compared)", name, n)
}

// Driver returns the GPU driver. win maps a physical window; win(0) is
// all of physical memory (page tables are reached by physical address).
//
// Stage 1 brings the GPU up to the point where the firmware and the
// kernel share the UAT: power, identity, coprocessor CPU, handoff, the
// kernel's page tables in slot 0. Stage 2 builds InitData and the
// channels, boots RTKit and initializes the firmware, then relays its log
// and watches for MMU faults (docs/AGX.md).
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

	log                              fwLog
	doorbells, events, stats, ktrace int
	unknownMsgs                      int
	calib                            []string // InitData vs m1n1's calibration blobs
	kalloc                           *KernelAlloc
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
	if g.kalloc != nil {
		out = append(out, fmt.Sprintf("kernel GPU memory: %s; doorbells %d, events %d, stats %d, ktrace %d, unknown %d",
			g.kalloc.Used(), g.doorbells, g.events, g.stats, g.ktrace, g.unknownMsgs))
	}
	out = append(out, g.calib...)
	g.log.mu.Lock()
	out = append(out, fmt.Sprintf("firmware log: %d lines (%d malformed)", g.log.total, g.log.dropped))
	if n := len(g.log.lines); n > 0 {
		out = append(out, "  last: "+g.log.lines[n-1])
	}
	g.log.mu.Unlock()
	return out
}

// Log returns the firmware's recent log lines.
func (g *GPU) Log() []string {
	g.log.mu.Lock()
	defer g.log.mu.Unlock()
	return append([]string(nil), g.log.lines...)
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
	g.stage = "UAT handed off"
	d.Ready(g)

	// Stage 2: the firmware's memory, channels and InitData.
	L, err := fw.For(int(hw.Gen), c.Compat)
	if err != nil {
		return err
	}
	k := NewKernelAlloc(kern, d.DMA)
	g.mu.Lock()
	g.kalloc = k
	g.mu.Unlock()
	ch, err := NewChannels(k, c.Compat)
	if err != nil {
		return fmt.Errorf("agx: channels: %w", err)
	}
	init, err := BuildInitData(k, lower, L, hw, id, c.Pwr, c.Compat, c.TTBs.Base, ch)
	if err != nil {
		return fmt.Errorf("agx: InitData: %w", err)
	}
	calib := []string{
		compareBlob("HwDataA", init.HwA, L.Get("initdata.HwDataA").Size, c.CalA, win),
		compareBlob("HwDataB", init.HwB, L.Get("initdata.HwDataB").Size, c.CalB, win),
		compareBlob("Globals", init.Globals, L.Get("initdata.Globals").Size, c.CalGlobals, win),
	}
	g.mu.Lock()
	g.calib = calib
	g.mu.Unlock()
	g.setStage("InitData built; booting RTKit")

	// RTKit, as for the SSD: the mailbox and RTKit outlive ctx so the
	// firmware can be put to sleep on halt.
	mctx, stopMbox := context.WithCancel(context.WithoutCancel(ctx))
	defer stopMbox()
	mb := asc.New(win(c.Mailbox))
	line := asc.RecvLine(d.Bridge, c.MailboxIRQs, "agx mailbox")
	d.Proc.Go(func(context.Context) { mb.Run(mctx, line) })
	rk := rtkit.New("agx", mb, d.DMA, RTKitIOMMU{k})
	d.Proc.Go(func(context.Context) { rk.Run(mctx) })

	// Queue Initialize before booting, as drm/asahi does.
	op := make([]byte, DeviceControlMsgSize)
	binary.LittleEndian.PutUint32(op, initializeOp(c.Compat))
	if _, err := ch.DeviceControl.Put(ctx, op); err != nil {
		return err
	}
	bctx, cancel := context.WithTimeout(ctx, rtkitBootTimeout)
	err = rk.Boot(bctx)
	cancel()
	if err != nil {
		return fmt.Errorf("agx: %s: %w (mailbox %s)", id, err, mb.Diag())
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		rk.Shutdown(sctx)
		cancel()
	}()
	fwRecv, fwSend, err := rk.Endpoint(ctx, EPFirmware)
	if err != nil {
		return err
	}
	_, dbSend, err := rk.Endpoint(ctx, EPDoorbell)
	if err != nil {
		return err
	}
	d.Proc.Go(func(ctx context.Context) { firmwareLoop(ctx, fwRecv, ch, g) })
	if err := fwSend(ctx, MsgInit|init.Root.VA&initDataMask); err != nil {
		return err
	}
	if err := dbSend(ctx, MsgTxDoorbell|DoorbellDevCtrl); err != nil {
		return err
	}
	if err := dbSend(ctx, MsgTxDoorbell|DoorbellKickFW); err != nil {
		return err
	}
	g.setStage(fmt.Sprintf("firmware initialized (InitData at %#x); work submission: stage 3", init.Root.VA))

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
