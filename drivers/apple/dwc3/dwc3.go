// Package dwc3 runs the M1 Pro's USB-C ports in USB 2 host mode: Apple's
// glue around a Synopsys DWC3 core, after m1n1's usb.c and Linux's
// dwc3-apple.c and phy-apple-atc.c (only their USB 2 parts).
//
// Per port: a CD321x USB-C port controller on I2C (polled here for plug
// state), the Type-C PHY's USB 2 half and its "pipehandler" (the PIPE mux,
// left on its dummy backend: no SuperSpeed), the DWC3 core whose xHCI
// registers start at offset 0, and two DARTs in front of it.
//
// The eUSB2 repeater between the port and the PHY is reset by the port
// controller on every cable change, and the PHY then has to be reset too:
// so for every cable, the driver brings the PHY and the core up, runs a
// fresh xHCI instance (drivers/usb/xhci) until the cable goes, and takes
// everything down again. What a cable brings (keyboard, disk, Ethernet
// adapter) keeps its device name across replugs (xhci.Names).
package dwc3

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"godeep/drivers/apple/dart"
	"godeep/drivers/apple/i2c"
	"godeep/drivers/apple/pmgr"
	"godeep/drivers/usb/xhci"
	"godeep/kernel/dev"
	"godeep/kernel/input"
	"godeep/kernel/mmio"
)

// Compatible is the controller in the M1 Pro's device tree.
const Compatible = "apple,t6000-dwc3"

// DWC3 global registers (from the core's base) and Apple's.
const (
	GCTL         = 0xc110
	GSNPSID      = 0xc120
	GUSB2PHYCFG  = 0xc200
	GUSB3PIPECTL = 0xc2c0

	GCTL_PRTCAPDIR_MASK = 3 << 12
	GCTL_PRTCAP_HOST    = 1 << 12
	GCTL_CORESOFTRESET  = 1 << 11
	GCTL_SCALEDOWN_MASK = 3 << 4
	GCTL_DISSCRAMBLE    = 1 << 3
	USB2PHY_SUSPHY      = 1 << 6
	USB3PIPE_SUSPHY     = 1 << 17
	PHYSOFTRST          = 1 << 31
	snpsidMask          = 0xffff0000
	snpsidDWC3          = 0x33310000

	CIO_LFPS_OFFSET   = 0xcd38
	CIO_BW_NGT_OFFSET = 0xcd3c
	CIO_LINK_TIMER    = 0xcd40
)

// USB 2 PHY registers (the Type-C PHY's "usb2phy" window).
const (
	USB2PHY_USBCTL   = 0x00
	USB2PHY_CTL      = 0x04
	USB2PHY_SIG      = 0x08
	USB2PHY_MISCTUNE = 0x1c

	USBCTL_RUN       = 2
	USBCTL_ISOLATION = 4
	CTL_RESET        = 1 << 0
	CTL_PORT_RESET   = 1 << 1
	CTL_APB_RESET_N  = 1 << 2
	CTL_SIDDQ        = 1 << 3
	SIG_VBUS_FORCE   = 0xf // VBUSDET and VBUSVLDEXT forced valid
	SIG_HOST         = 7 << 12
	MISCTUNE_GATES   = 3 << 29 // APB and reference clock gates off
)

// Pipehandler registers.
const (
	PIPE_MUX_CTRL      = 0x0c
	PIPE_AON_GEN       = 0x1c
	PIPE_NONSEL_OVR    = 0x20
	PIPE_MUX_DUMMY     = 0x22 // clock and data from the dummy PHY: USB 2 only
	PIPE_DWC3_RESET_N  = 1 << 0
	PIPE_DWC3_CLAMP_EN = 1 << 4
	pipeNonselDummy    = 0x9332 // m1n1's value with the dummy PHY
)

// HPM (CD321x) STATUS register.
const (
	hpmStatus      = 0x1a
	hpmPlugPresent = 1 << 0
	hpmDataRoleDFP = 1 << 6 // we are the host
	HPMPoll        = 250 * time.Millisecond
)

// IOVABase is where the board's DMA region appears to the controller.
const IOVABase = 0x1000_0000

// Stream is one DART and the stream ID the controller uses on it.
type Stream struct {
	Base   uint64
	Stream int
}

// Config is what the board resolves from the device tree.
type Config struct {
	Label       string // "USB-C Left Back"
	Core        uint64 // DWC3 registers (xHCI at +0)
	USB2PHY     uint64
	Pipehandler uint64
	DARTs       []Stream
	Power       []pmgr.Domain
	I2C         string // the port controller's bus (device name); "": none, assume a cable
	HPM         byte   // the port controller's I2C address
	Keys        chan<- byte
	Names       *xhci.Names // shared by every port
}

// Driver returns the DWC3 driver; regs maps register windows.
func Driver(regs func(base uint64) mmio.Regs) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		switch c := d.Config.(type) {
		case Config:
			return run(ctx, d, regs, c)
		case error:
			return c
		}
		return errors.New("dwc3: no configuration from the board")
	}}
}

// Service is the port's lsdev entry; it passes on what the current xHCI
// instance offers (devices, input sources, keyboard layout).
type Service struct {
	mu     sync.Mutex
	label  string
	state  string
	hpm    string
	fault  string
	cables int
	hc     *xhci.Service
	layout *input.Layout
}

func (s *Service) set(f func(*Service)) { s.mu.Lock(); f(s); s.mu.Unlock() }

func (s *Service) Devices() []string {
	s.mu.Lock()
	hc := s.hc
	l := fmt.Sprintf("%s: %s", s.label, s.state)
	if s.hpm != "" {
		l += "; port controller " + s.hpm
	}
	if s.cables > 0 {
		l += fmt.Sprintf("; %d cables", s.cables)
	}
	if s.fault != "" {
		l += "; DART " + s.fault
	}
	s.mu.Unlock()
	out := []string{l}
	if hc != nil {
		out = append(out, hc.Devices()...)
	}
	return out
}

func (s *Service) InputSources() []input.Source {
	s.mu.Lock()
	hc := s.hc
	s.mu.Unlock()
	if hc == nil {
		return nil
	}
	return hc.InputSources()
}

func (s *Service) SetLayout(l *input.Layout) {
	s.mu.Lock()
	s.layout = l
	hc := s.hc
	s.mu.Unlock()
	if hc != nil {
		hc.SetLayout(l)
	}
}

type port struct {
	core, phy, pipe mmio.Regs
}

func run(ctx context.Context, d *dev.Device, regs func(uint64) mmio.Regs, c Config) error {
	if d.DMA == nil {
		return errors.New("dwc3: the board has no DMA region")
	}
	for _, p := range c.Power {
		if _, err := pmgr.On(ctx, regs(p.Addr)); err != nil {
			return fmt.Errorf("dwc3: power domain %s: %w", p.Name, err)
		}
	}
	svc := &Service{label: c.Label, state: "starting"}
	if svc.label == "" {
		svc.label = d.Name
	}

	// Map the whole DMA region for the controller, once.
	alloc := d.DMA
	var darts []mmio.Regs
	if len(c.DARTs) > 0 {
		base, size := d.DMA.Region()
		tables, err := dart.Build(d.DMA, base, size&^(dart.PageSize-1), IOVABase)
		if err != nil {
			return err
		}
		defer tables.Free()
		for _, s := range c.DARTs {
			r := regs(s.Base)
			if err := dart.Attach(r, s.Stream, tables); err != nil {
				return fmt.Errorf("dwc3: DART at %#x: %w", s.Base, err)
			}
			darts = append(darts, r)
		}
		alloc = d.DMA.Translated(IOVABase)
	}

	var bus *i2c.Bus
	if c.I2C != "" {
		v, err := d.Lookup(ctx, c.I2C)
		if err != nil {
			return fmt.Errorf("dwc3: port controller bus: %w", err)
		}
		if bus, _ = v.(*i2c.Bus); bus == nil {
			return fmt.Errorf("dwc3: %s is not an I2C bus", c.I2C)
		}
	}
	p := &port{core: regs(c.Core), phy: regs(c.USB2PHY), pipe: regs(c.Pipehandler)}
	p.resetAssert()
	d.Ready(svc)

	// plugged reports whether a cable is in with us as the host.
	plugged := func() (bool, error) {
		if bus == nil {
			return true, nil
		}
		b, err := bus.ReadReg(ctx, c.HPM, hpmStatus, 4)
		if err != nil || len(b) < 4 {
			svc.set(func(s *Service) { s.hpm = fmt.Sprintf("unreadable (%v)", err) })
			return false, err
		}
		st := binary.LittleEndian.Uint32(b)
		desc := "no cable"
		switch {
		case st&hpmPlugPresent != 0 && st&hpmDataRoleDFP != 0:
			desc = "cable in, host"
		case st&hpmPlugPresent != 0:
			desc = "cable in, device role (not supported yet)"
		}
		svc.set(func(s *Service) { s.hpm = fmt.Sprintf("%s (status %#x)", desc, st) })
		return st&hpmPlugPresent != 0 && st&hpmDataRoleDFP != 0, nil
	}
	fault := func() {
		for _, r := range darts {
			if f := dart.Fault(r); f != "" {
				svc.set(func(s *Service) { s.fault = f })
			}
		}
	}

	for {
		svc.set(func(s *Service) { s.state, s.hc = "waiting for a cable", nil })
		for {
			if ok, _ := plugged(); ok {
				break
			}
			if err := sleep(ctx, HPMPoll); err != nil {
				return nil
			}
		}
		svc.set(func(s *Service) { s.state = "bringing up"; s.cables++ })
		cctx, cancel := context.WithCancel(ctx)
		d.Proc.Go(func(context.Context) { // watch the cable
			defer cancel()
			for sleep(cctx, HPMPoll) == nil {
				fault()
				if ok, err := plugged(); !ok && err == nil {
					return
				}
			}
		})
		err := p.up(cctx)
		if err == nil {
			err = xhci.Run(cctx, d, xhci.Config{Regs: p.core, DMA: alloc, Keys: c.Keys, Names: c.Names,
				Ready: func(hc *xhci.Service) {
					svc.set(func(s *Service) {
						s.state, s.hc = "host, USB 2", hc
						if s.layout != nil {
							hc.SetLayout(s.layout)
						}
					})
				}})
		}
		cancel()
		p.resetAssert()
		fault()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			svc.set(func(s *Service) { s.state, s.hc = "failed: "+err.Error(), nil })
			sleep(ctx, time.Second) // then wait for the cable to go and come back
			for {
				if ok, _ := plugged(); !ok {
					break
				}
				if sleep(ctx, HPMPoll) != nil {
					return nil
				}
			}
		}
	}
}

// resetAssert holds the core in reset and powers the USB 2 PHY down
// (Linux's atcphy_dwc3_reset_assert).
func (p *port) resetAssert() {
	mmio.Modify(p.pipe, PIPE_AON_GEN, PIPE_DWC3_RESET_N, 0)
	mmio.Modify(p.pipe, PIPE_AON_GEN, 0, PIPE_DWC3_CLAMP_EN)
	p.pipe.Write32(PIPE_MUX_CTRL, PIPE_MUX_DUMMY)
	p.pipe.Write32(PIPE_NONSEL_OVR, pipeNonselDummy)

	mmio.Modify(p.phy, USB2PHY_USBCTL, 7, USBCTL_ISOLATION)
	udelay()
	mmio.Modify(p.phy, USB2PHY_CTL, 0, CTL_SIDDQ)
	udelay()
	mmio.Modify(p.phy, USB2PHY_CTL, 0, CTL_PORT_RESET)
	udelay()
	mmio.Modify(p.phy, USB2PHY_CTL, 0, CTL_RESET)
	udelay()
	mmio.Modify(p.phy, USB2PHY_CTL, CTL_APB_RESET_N, 0)
	udelay()
	mmio.Modify(p.phy, USB2PHY_MISCTUNE, 0, MISCTUNE_GATES)
}

// up powers the USB 2 PHY in host mode, releases the core and sets it up
// as a host (Linux: atcphy_usb2_power_on, phy_set_mode, reset deassert,
// dwc3_core_init, dwc3_apple_setup_cio, PRTCAP host, SUSPHY).
func (p *port) up(ctx context.Context) error {
	mmio.Modify(p.phy, USB2PHY_SIG, 0, SIG_VBUS_FORCE)
	udelay()
	mmio.Modify(p.phy, USB2PHY_CTL, CTL_SIDDQ, 0)
	udelay()
	mmio.Modify(p.phy, USB2PHY_CTL, CTL_RESET, 0)
	udelay()
	mmio.Modify(p.phy, USB2PHY_CTL, CTL_PORT_RESET, 0)
	udelay()
	mmio.Modify(p.phy, USB2PHY_CTL, 0, CTL_APB_RESET_N)
	udelay()
	mmio.Modify(p.phy, USB2PHY_MISCTUNE, MISCTUNE_GATES, 0)
	p.phy.Write32(USB2PHY_USBCTL, USBCTL_RUN)
	mmio.Modify(p.phy, USB2PHY_SIG, 0, SIG_HOST)

	mmio.Modify(p.pipe, PIPE_AON_GEN, PIPE_DWC3_CLAMP_EN, 0)
	mmio.Modify(p.pipe, PIPE_AON_GEN, 0, PIPE_DWC3_RESET_N)

	r := p.core
	if id := r.Read32(GSNPSID); id&snpsidMask != snpsidDWC3 {
		return fmt.Errorf("dwc3: no DWC3 core (GSNPSID %#x)", id)
	}
	// Soft reset of the core and its PHY interfaces (m1n1's sequence).
	mmio.Modify(r, GCTL, 0, GCTL_CORESOFTRESET)
	mmio.Modify(r, GUSB3PIPECTL, 0, PHYSOFTRST)
	mmio.Modify(r, GUSB2PHYCFG, 0, PHYSOFTRST)
	if err := sleep(ctx, 20*time.Millisecond); err != nil {
		return err
	}
	mmio.Modify(r, GUSB3PIPECTL, PHYSOFTRST, 0)
	mmio.Modify(r, GUSB2PHYCFG, PHYSOFTRST, 0)
	if err := sleep(ctx, 20*time.Millisecond); err != nil {
		return err
	}
	mmio.Modify(r, GCTL, GCTL_CORESOFTRESET, 0)
	if err := sleep(ctx, 20*time.Millisecond); err != nil {
		return err
	}
	mmio.Modify(r, GCTL, GCTL_SCALEDOWN_MASK|GCTL_DISSCRAMBLE|GCTL_PRTCAPDIR_MASK, GCTL_PRTCAP_HOST)

	r.Write32(CIO_LFPS_OFFSET, 0xf800f80)
	r.Write32(CIO_BW_NGT_OFFSET, 0xfc00fc0)
	mmio.Modify(r, CIO_LINK_TIMER, 0xffffff, 0x14<<16|0xa<<8|0x10)

	mmio.Modify(r, GUSB2PHYCFG, 0, USB2PHY_SUSPHY)
	mmio.Modify(r, GUSB3PIPECTL, 0, USB3PIPE_SUSPHY)
	return nil
}

func udelay() { time.Sleep(20 * time.Microsecond) }

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Summary is a one-line description of a Config, for boot info.
func (c Config) Summary() string {
	var darts []string
	for _, s := range c.DARTs {
		darts = append(darts, fmt.Sprintf("%#x/%d", s.Base, s.Stream))
	}
	return fmt.Sprintf("%s: core %#x, usb2phy %#x, pipe %#x, DARTs %s, HPM %s@%#x",
		c.Label, c.Core, c.USB2PHY, c.Pipehandler, strings.Join(darts, " "), c.I2C, c.HPM)
}
