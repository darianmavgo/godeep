// Package spihid drives the internal keyboard of Apple Silicon laptops,
// HID over SPI ("apple,spi-hid-transport"), by polling, after U-Boot's
// apple_spi_kbd.c (U-Boot has no interrupts either). Only the keyboard is
// handled; trackpad packets are counted and dropped.
//
// Bring-up, all from the device tree (fdt.go): power on the SPI controller's
// and the GPIO blocks' power domains, mux the SPI pins, set up the
// controller, then pulse the keyboard's enable pin (spien) 1-0-1 with 5 ms
// steps. After that the keyboard sends input reports, no commands needed.
//
// Polling: the keyboard pulls its interrupt line (a GPIO on the always-on
// "nub" block, active low) while it has a packet. Every PollInterval the
// driver reads that pin's level and fetches packets while it's asserted.
// If the line never asserts within ReadyTimeout of power-on (the keyboard
// sends a "booted" packet then), the driver falls back to U-Boot's method:
// a blind read every BlindInterval. lsdev shows which mode it's in.
package spihid

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"godeep/drivers/apple/gpio"
	"godeep/drivers/apple/pmgr"
	"godeep/drivers/apple/spi"
	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// readyTimeout is ReadyTimeout; tests shorten it.
var readyTimeout = ReadyTimeout

// Compatible is the transport node's compatible string.
const Compatible = "apple,spi-hid-transport"

const (
	PollInterval  = 5 * time.Millisecond
	BlindInterval = 10 * time.Millisecond
	ReadyTimeout  = 3 * time.Second
	// Packets fetched per poll at most, so a stuck line can't hog the core.
	maxBurst = 8
	// Consecutive SPI failures before the driver gives up (and devmgr
	// restarts it).
	maxErrors = 50
)

// Config is what the board resolves from the device tree.
type Config struct {
	SPI     uint64 // controller registers
	ClockHz uint64 // controller input clock
	SpeedHz uint64 // bus clock
	// Chip-select timing.
	CSSetup, CSHold, CSInactive time.Duration

	Power  []pmgr.Domain // parents first
	Mux    []gpio.Mux    // the controller's pins
	Enable gpio.Pin      // spien: keyboard power/enable
	// Ready is the keyboard's interrupt line, polled; nil: blind reads.
	Ready *gpio.Pin
	// ReadyActiveHigh: the line is asserted high (default: low).
	ReadyActiveHigh bool

	// Keys receives the typed bytes (sends never block: a full channel
	// drops the byte, counted in lsdev).
	Keys chan<- byte
}

// Driver returns the keyboard driver. regs maps a register base to its
// window (mmio.Window on hardware, fakes in tests).
func Driver(regs func(base uint64) mmio.Regs) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		switch c := d.Config.(type) {
		case error:
			return c
		case Config:
			return run(ctx, d, regs, c)
		}
		return fmt.Errorf("spihid: no configuration from the board (%T)", d.Config)
	}}
}

func run(ctx context.Context, d *dev.Device, regs func(uint64) mmio.Regs, c Config) error {
	st := &Stats{}
	for _, dom := range c.Power {
		sw, err := pmgr.On(ctx, regs(dom.Addr))
		if err != nil {
			return fmt.Errorf("power domain %s (%#x): %w", dom.Name, dom.Addr, err)
		}
		st.domains++
		if sw {
			st.switched++
		}
	}
	for _, m := range c.Mux {
		if (gpio.Block{R: regs(m.Block)}).Mux(m.Pin.Pin, m.Func) {
			st.muxed++
		}
	}
	ctl := spi.Controller{R: regs(c.SPI)}
	ctl.Init()
	st.hz = ctl.SetSpeed(c.ClockHz, c.SpeedHz)

	var ready gpio.Block
	if c.Ready != nil {
		ready = gpio.Block{R: regs(c.Ready.Block)}
		if !ready.IsInput(c.Ready.Pin) {
			ready.Input(c.Ready.Pin)
			st.note("ready pin set to input")
		}
	}

	en := gpio.Block{R: regs(c.Enable.Block)}
	for _, v := range []bool{true, false, true} {
		en.Set(c.Enable.Pin, v)
		if err := sleep(ctx, 5*time.Millisecond); err != nil {
			return err
		}
	}
	poweredOn := time.Now()

	k := &keyboard{ctx: ctx, c: c, ctl: ctl, st: st}
	k.dec.Emit = k.emit
	st.set(func() {
		st.mode = "blind reads"
		if c.Ready != nil {
			st.mode = "ready pin"
		}
	})
	d.Ready(st)

	gated := c.Ready != nil
	interval := PollInterval
	if !gated {
		interval = BlindInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			if gated && !k.sawReady && now.Sub(poweredOn) > readyTimeout {
				gated = false
				t.Reset(BlindInterval)
				st.set(func() { st.mode = "blind reads (ready pin never asserted)" })
			}
			for i := 0; i < maxBurst; i++ {
				if gated {
					if ready.Get(c.Ready.Pin) != c.ReadyActiveHigh {
						break
					}
					k.sawReady = true
				} else if i > 0 {
					break
				}
				if err := k.read(); err != nil {
					return err
				}
			}
			k.dec.Tick(now)
		}
	}
}

type keyboard struct {
	ctx      context.Context
	c        Config
	ctl      spi.Controller
	st       *Stats
	dec      Decoder
	buf      [PacketSize]byte
	sawReady bool
	errors   int
}

// read fetches and handles one packet.
func (k *keyboard) read() error {
	k.ctl.CS(true)
	sleep(k.ctx, k.c.CSSetup)
	err := k.ctl.Transfer(k.ctx, nil, k.buf[:])
	sleep(k.ctx, k.c.CSHold)
	k.ctl.CS(false)
	sleep(k.ctx, k.c.CSInactive)
	if err != nil {
		k.errors++
		k.st.set(func() { k.st.spiErrors++; k.st.lastErr = err.Error() })
		if k.errors >= maxErrors {
			return fmt.Errorf("%d SPI errors in a row, last: %w", k.errors, err)
		}
		return nil
	}
	k.errors = 0
	if Empty(k.buf[:]) {
		k.st.set(func() { k.st.empty++ })
		return nil
	}
	p, _ := ParsePacket(k.buf[:])
	r, isKbd := p.KeyboardReport()
	k.st.set(func() {
		k.st.packets++
		if p.CRCOK {
			k.st.crcOK++
		}
		if isKbd {
			k.st.kbd++
		} else if k.st.firstOther == "" {
			k.st.firstOther = hex.EncodeToString(k.buf[:12])
		}
	})
	if isKbd {
		k.dec.Report(r, time.Now())
	}
	return nil
}

func (k *keyboard) emit(b []byte) {
	sent, dropped := 0, 0
	for _, x := range b {
		select {
		case k.c.Keys <- x:
			sent++
		default:
			dropped++
		}
	}
	k.st.set(func() { k.st.keys += sent; k.st.dropped += dropped })
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Stats is the driver's service: lsdev shows it (dev.Lister).
type Stats struct {
	mu                         sync.Mutex
	domains, switched, muxed   int
	hz                         uint64
	mode                       string
	packets, kbd, crcOK, empty int
	spiErrors, keys, dropped   int
	firstOther, lastErr        string
	notes                      []string
}

func (s *Stats) set(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

func (s *Stats) note(n string) { s.set(func() { s.notes = append(s.notes, n) }) }

// Devices reports bring-up and traffic, e.g. "ready pin; 7 power domains
// (2 switched on), 0 pins muxed, 8 MHz; 12 packets (10 keyboard, crc ok
// 12), 30 empty; 25 keys".
func (s *Stats) Devices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{
		s.mode,
		fmt.Sprintf("%d power domains (%d switched on), %d pins muxed, %d MHz", s.domains, s.switched, s.muxed, s.hz/1e6),
		fmt.Sprintf("%d packets (%d keyboard, crc ok %d), %d empty", s.packets, s.kbd, s.crcOK, s.empty),
		fmt.Sprintf("%d keys (%d dropped)", s.keys, s.dropped),
	}
	if s.spiErrors > 0 {
		out = append(out, fmt.Sprintf("%d SPI errors, last: %s", s.spiErrors, s.lastErr))
	}
	if s.firstOther != "" {
		out = append(out, "first other packet: "+s.firstOther)
	}
	if len(s.notes) > 0 {
		out = append(out, strings.Join(s.notes, ", "))
	}
	return out
}
