// Package spihid drives the internal keyboard and trackpad of Apple
// Silicon laptops, HID over SPI ("apple,spi-hid-transport"), by polling,
// after U-Boot's apple_spi_kbd.c (U-Boot has no interrupts either) for the
// keyboard and Asahi's spi-hid-apple-core.c + hid-magicmouse.c for the
// trackpad. Keys become console bytes; both devices publish input events
// (input.Sources).
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
//
// The trackpad needs one write: a set-report switching multitouch mode on,
// sent once the device says it booted (or after MTDelay), and again if the
// touch controller reports a reset.
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
	"godeep/kernel/input"
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
	// Without a "booted" packet, switch multitouch on this long after
	// power-on anyway.
	MTDelay = time.Second
	// Most multitouch set-reports sent (resets re-send).
	maxMTWrites = 8
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
	// Trackpad: switch the trackpad to multitouch mode (one write to the
	// device). Without it the driver only reads.
	Trackpad bool
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
	st := &Stats{kbdEvents: &input.Broadcaster{Kind: "keyboard"}, tpEvents: &input.Broadcaster{Kind: "trackpad"}}
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

	k := &transport{ctx: ctx, c: c, ctl: ctl, st: st}
	k.kbd = input.Keyboard{Emit: k.emit, Events: st.kbdEvents}
	k.tp = Trackpad{Events: st.tpEvents}
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
			if c.Trackpad && k.mtWrites < maxMTWrites && (k.wantMT || k.mtWrites == 0 && now.Sub(poweredOn) > MTDelay) {
				k.enableMT()
			}
			k.kbd.Tick(now)
		}
	}
}

// transport owns the SPI link to the keyboard and trackpad.
type transport struct {
	ctx      context.Context
	c        Config
	ctl      spi.Controller
	st       *Stats
	kbd      input.Keyboard
	tp       Trackpad
	asm      assembler
	buf      [PacketSize]byte
	sawReady bool
	errors   int
	msgID    byte
	wantMT   bool // booted or reset: send the multitouch set-report
	mtWrites int
}

// read fetches and handles one packet.
func (k *transport) read() error {
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
	now := time.Now()
	k.st.set(func() {
		k.st.packets++
		if p.CRCOK {
			k.st.crcOK++
		}
	})
	if p.IsBooted() {
		k.wantMT = true
		k.st.set(func() { k.st.booted++ })
		return nil
	}
	raw, done := k.asm.add(p)
	if !done {
		return nil
	}
	m, _ := ParseMessage(p.Flags, p.Device, raw)
	if r, ok := m.KeyboardReport(); ok {
		k.st.set(func() { k.st.kbd++ })
		k.kbd.State(r[1], r[3:9], now)
		return nil
	}
	if r, ok := m.TrackpadReport(); ok {
		ok, reset := k.tp.Report(r, now)
		if reset {
			k.wantMT = true
		}
		k.st.set(func() {
			k.st.tp++
			k.st.tpTouch = k.tp.touchMode
			if !ok && k.st.firstTP == "" {
				k.st.firstTP = hex.EncodeToString(r[:min(len(r), 16)])
			}
		})
		return nil
	}
	k.st.set(func() {
		if p.Flags == FlagWrite {
			k.st.replies++ // answers to our writes
		} else if k.st.firstOther == "" {
			k.st.firstOther = hex.EncodeToString(k.buf[:12])
		}
	})
	return nil
}

// enableMT sends the set-report that switches the trackpad to multitouch
// mode: a write packet, then (chip select held) the 4-byte status.
func (k *transport) enableMT() {
	k.wantMT = false
	k.mtWrites++
	pkt := WritePacket(DevTP, MsgSetReport, mtEnable[0], 0, k.msgID, 2, mtEnable)
	k.msgID++
	status := make([]byte, 4)
	k.ctl.CS(true)
	sleep(k.ctx, k.c.CSSetup)
	err := k.ctl.Transfer(k.ctx, pkt, nil)
	if err == nil {
		sleep(k.ctx, 200*time.Microsecond)
		err = k.ctl.Transfer(k.ctx, nil, status)
	}
	sleep(k.ctx, k.c.CSHold)
	k.ctl.CS(false)
	sleep(k.ctx, k.c.CSInactive)
	res := "status ok"
	switch {
	case err != nil:
		res = err.Error()
	case string(status) != string(StatusOK):
		res = "status " + hex.EncodeToString(status)
	}
	n := k.mtWrites
	k.st.set(func() { k.st.mtWrites, k.st.mtResult = n, res })
}

func (k *transport) emit(b []byte) {
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

// Stats is the driver's service: lsdev shows it (dev.Lister), and the
// keyboard's and trackpad's events come from it (input.Sources).
type Stats struct {
	kbdEvents, tpEvents *input.Broadcaster

	booted, tp, replies, mtWrites int
	tpTouch                       bool
	firstTP, mtResult             string

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

// InputSources implements input.Sources.
func (s *Stats) InputSources() []input.Source { return []input.Source{s.kbdEvents, s.tpEvents} }

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
	tp := fmt.Sprintf("trackpad: %d reports", s.tp)
	if s.tpTouch {
		tp += " (multitouch)"
	}
	if s.mtWrites > 0 {
		tp += fmt.Sprintf(", multitouch on: %d sent, %s", s.mtWrites, s.mtResult)
	}
	if s.booted > 0 {
		tp += fmt.Sprintf(", booted packets %d", s.booted)
	}
	if s.replies > 0 {
		tp += fmt.Sprintf(", %d replies", s.replies)
	}
	out = append(out, tp)
	if s.firstTP != "" {
		out = append(out, "first unrecognized trackpad report: "+s.firstTP)
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
