package spihid

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"godeep/drivers/apple/gpio"
	"godeep/drivers/apple/pmgr"
	"godeep/drivers/apple/spi"
	"godeep/kernel/dev"
	"godeep/kernel/fdt"
	"godeep/kernel/input"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
)

// Addresses as in Asahi's t600x device trees.
const (
	pmgrBase = 0x2_8e08_0000
	apBase   = 0x3_9b02_8000
	nubBase  = 0x2_921f_0000
	spiBase  = 0x3_9b10_c000
	spienPin = 194
	readyPin = 6
)

// t6000Tree is the part of the j316s tree the keyboard needs.
func t6000Tree(t *testing.T) *fdt.Node {
	var b fdt.Builder
	b.Begin("")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Begin("clock-ref")
	b.PropStrings("compatible", "fixed-clock")
	b.PropU32("#clock-cells", 0)
	b.PropU32("clock-frequency", 24_000_000)
	b.PropU32("phandle", 1)
	b.End()
	b.Begin("soc")
	b.PropStrings("compatible", "simple-bus")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Prop("ranges", nil)

	b.Begin("power-management@28e080000")
	b.PropStrings("compatible", "apple,t6000-pmgr", "apple,pmgr", "syscon", "simple-mfd")
	b.PropRegs(2, 2, fdt.Range{Base: pmgrBase, Size: 0x4000})
	b.PropU32("#address-cells", 1)
	b.PropU32("#size-cells", 1)
	dom := func(off, ph uint32, parents ...uint32) {
		b.Begin(fmt.Sprintf("power-controller@%x", off))
		b.PropStrings("compatible", "apple,t6000-pmgr-pwrstate", "apple,pmgr-pwrstate")
		b.PropU32("reg", off, 4)
		b.PropU32("#power-domain-cells", 0)
		b.PropU32("phandle", ph)
		if len(parents) > 0 {
			b.PropU32("power-domains", parents...)
		}
		b.End()
	}
	dom(0x1d8, 10)     // ps_afi (always on)
	dom(0x148, 11, 10) // ps_afnc2_ioa
	dom(0x150, 12, 11) // ps_afnc2_ls
	dom(0x160, 13, 12) // ps_afnc2_lw1
	dom(0x180, 14, 13) // ps_sio
	dom(0x1e8, 15, 14) // ps_spi_p
	dom(0x208, 16, 15) // ps_spi3
	dom(0x120, 17, 14) // ps_gpio
	b.End()

	b.Begin("pinctrl@39b028000")
	b.PropStrings("compatible", "apple,t6000-pinctrl", "apple,pinctrl")
	b.PropRegs(2, 2, fdt.Range{Base: apBase, Size: 0x4000})
	b.PropU32("apple,npins", 255)
	b.PropU32("#gpio-cells", 2)
	b.PropU32("#interrupt-cells", 2)
	b.PropU32("power-domains", 17)
	b.PropU32("phandle", 20)
	b.Begin("spi3-pins")
	b.PropU32("pinmux", 52|1<<16, 53|1<<16, 54|1<<16, 55|1<<16)
	b.PropU32("phandle", 21)
	b.End()
	b.End()

	b.Begin("pinctrl@2921f0000")
	b.PropStrings("compatible", "apple,t6000-pinctrl", "apple,pinctrl")
	b.PropRegs(2, 2, fdt.Range{Base: nubBase, Size: 0x4000})
	b.PropU32("apple,npins", 16)
	b.PropU32("#gpio-cells", 2)
	b.PropU32("#interrupt-cells", 2)
	b.PropU32("phandle", 22)
	b.End()

	b.Begin("spi@39b10c000")
	b.PropStrings("compatible", "apple,t6000-spi", "apple,spi")
	b.PropRegs(2, 2, fdt.Range{Base: spiBase, Size: 0x4000})
	b.PropU32("#address-cells", 1)
	b.PropU32("#size-cells", 0)
	b.PropU32("clocks", 1)
	b.PropU32("pinctrl-0", 21)
	b.PropStrings("pinctrl-names", "default")
	b.PropU32("power-domains", 16)
	b.PropStrings("status", "okay")
	b.Begin("hid-transport@0")
	b.PropStrings("compatible", "apple,spi-hid-transport")
	b.PropU32("reg", 0)
	b.PropU32("spi-max-frequency", 8_000_000)
	b.PropU32("spi-cs-setup-delay-ns", 65000)
	b.PropU32("spi-cs-hold-delay-ns", 65000)
	b.PropU32("spi-cs-inactive-delay-ns", 250000)
	b.PropU32("spien-gpios", 20, spienPin, 0)
	b.PropU32("interrupts-extended", 22, readyPin, 8) // IRQ_TYPE_LEVEL_LOW
	b.End()
	b.End()

	b.End()
	b.End()
	dtb, err := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	root, err := fdt.Parse(dtb)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestConfigFromFDT(t *testing.T) {
	root := t6000Tree(t)
	c, err := ConfigFromFDT(root.Find("/soc/spi@39b10c000/hid-transport@0"))
	if err != nil {
		t.Fatal(err)
	}
	if c.SPI != spiBase || c.ClockHz != 24_000_000 || c.SpeedHz != 8_000_000 || c.CSInactive != 250*time.Microsecond {
		t.Fatalf("controller: %+v", c)
	}
	if c.Enable != (gpio.Pin{Block: apBase, Pin: spienPin}) {
		t.Fatalf("spien: %+v", c.Enable)
	}
	if c.Ready == nil || c.Ready.Block != nubBase || c.Ready.Pin != readyPin || c.ReadyActiveHigh {
		t.Fatalf("ready: %+v high=%v", c.Ready, c.ReadyActiveHigh)
	}
	if len(c.Mux) != 4 || c.Mux[0].Pin.Pin != 52 || c.Mux[0].Func != 1 {
		t.Fatalf("mux: %+v", c.Mux)
	}
	// ps_gpio's chain, then ps_spi3's without the shared parents
	want := []uint64{0x1d8, 0x148, 0x150, 0x160, 0x180, 0x120, 0x1e8, 0x208}
	if len(c.Power) != len(want) {
		t.Fatalf("power: %+v", c.Power)
	}
	for i, off := range want {
		if c.Power[i].Addr != pmgrBase+uint64(off) {
			t.Fatalf("power[%d] = %+v, want offset %#x", i, c.Power[i], off)
		}
	}

	if _, err := ConfigFromFDT(root.Find("/soc/spi@39b10c000")); err == nil {
		t.Fatal("accepted a node that isn't under an SPI controller")
	}
}

// machine models the hardware the driver touches: power domains that turn
// on when asked, GPIO blocks, and an SPI controller with a keyboard behind
// it that sends queued packets and asserts its ready line (low) meanwhile.
type machine struct {
	mu      sync.Mutex
	pm, ap  *mmio.Fake
	nub     *mmio.Fake
	spi     *mmio.Fake
	queue   [][]byte
	cur     []byte // packet being clocked out
	running bool
	spien   []bool
	ready   bool // false: never assert the ready line
	// writes from the driver: txcnt bytes being received, then a 4-byte
	// status read
	txcnt, rxcnt int
	tx           []byte
	writes       [][]byte
	status       bool
}

func newMachine(ready bool) *machine {
	m := &machine{pm: mmio.NewFake(), ap: mmio.NewFake(), nub: mmio.NewFake(), spi: mmio.NewFake(), ready: ready}
	m.pm.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		f.Set(off, v&^pmgr.PS_ACTUAL|(v&pmgr.PS_TARGET)<<4)
		return true
	}
	m.pm.Set(0x1d8, 0xff) // always-on parent already active
	m.ap.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		if off == 4*spienPin && (v&gpio.MODE)>>1 == gpio.MODE_OUT {
			m.mu.Lock()
			m.spien = append(m.spien, v&gpio.DATA != 0)
			m.mu.Unlock()
		}
		return false
	}
	m.nub.Set(4*readyPin, gpio.DATA) // line idle (high)
	m.spi.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch off {
		case spi.TXCNT:
			m.txcnt = int(v)
		case spi.RXCNT:
			m.rxcnt = int(v)
		case spi.TXDATA:
			m.tx = append(m.tx, byte(v))
			if len(m.tx) == m.txcnt {
				m.writes = append(m.writes, m.tx)
				m.tx, m.status = nil, true
				f.Set(spi.IF_XFER, spi.XFER_TXCOMPLETE)
			}
		case spi.CTRL:
			if v&(spi.CTRL_TX_RESET|spi.CTRL_RX_RESET) != 0 {
				f.Set(spi.IF_XFER, 0)
			}
			m.running = v&spi.CTRL_RUN != 0
			if m.running && m.cur == nil && m.rxcnt > 0 {
				switch {
				case m.status && m.rxcnt == 4:
					m.cur, m.status = append([]byte{}, StatusOK...), false
				case len(m.queue) > 0:
					m.cur, m.queue = m.queue[0], m.queue[1:]
				default:
					m.cur = make([]byte, PacketSize) // an empty read
				}
			}
		}
		return false
	}
	m.spi.OnRead = func(f *mmio.Fake, off uint64) (uint32, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch off {
		case spi.FIFOSTAT:
			if !m.running || m.cur == nil {
				return 0, true
			}
			return uint32(min(len(m.cur), spi.FifoDepth)) << 24, true
		case spi.RXDATA:
			b := m.cur[0]
			m.cur = m.cur[1:]
			if len(m.cur) == 0 {
				m.cur, m.running = nil, false
				m.updateReady()
			}
			return uint32(b), true
		}
		return 0, false
	}
	return m
}

func (m *machine) updateReady() {
	v := m.nub.Get(4*readyPin) | gpio.DATA
	if m.ready && len(m.queue) > 0 {
		v &^= gpio.DATA
	}
	m.nub.Set(4*readyPin, v)
}

func (m *machine) send(pkts ...[]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue = append(m.queue, pkts...)
	m.updateReady()
}

func (m *machine) regs(base uint64) mmio.Regs {
	switch {
	case base >= pmgrBase && base < pmgrBase+0x4000:
		return offset{m.pm, base - pmgrBase}
	case base == apBase:
		return m.ap
	case base == nubBase:
		return m.nub
	case base == spiBase:
		return m.spi
	}
	panic("access outside the modeled devices")
}

// offset is a window into a larger fake (one pmgr domain register).
type offset struct {
	f   *mmio.Fake
	off uint64
}

func (o offset) Read32(off uint64) uint32     { return o.f.Read32(o.off + off) }
func (o offset) Write32(off uint64, v uint32) { o.f.Write32(o.off+off, v) }

func startKeyboard(t *testing.T, m *machine, cfg any) (*dev.Manager, context.CancelFunc) {
	t.Helper()
	dev.Register(Driver(m.regs))
	mgr := dev.NewManager(proc.NewTable(), nil, nil)
	mgr.Backoff = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx, []dev.Node{{Name: "hid-transport@0", Compatible: Compatible, Config: cfg}})
	return mgr, cancel
}

func readKeys(t *testing.T, keys <-chan byte, n int) string {
	t.Helper()
	var got []byte
	for len(got) < n {
		select {
		case b := <-keys:
			got = append(got, b)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %q, waiting for %d bytes", got, n)
		}
	}
	return string(got)
}

func TestTyping(t *testing.T) {
	m := newMachine(true)
	cfg, err := ConfigFromFDT(t6000Tree(t).Find("/soc/spi@39b10c000/hid-transport@0"))
	if err != nil {
		t.Fatal(err)
	}
	keys := make(chan byte, 64)
	cfg.Keys = keys
	mgr, cancel := startKeyboard(t, m, cfg)
	defer cancel()
	svc, err := mgr.Get(context.Background(), "hid-transport@0")
	if err != nil {
		t.Fatal(err)
	}

	booted := make([]byte, PacketSize)
	copy(booted, []byte{FlagRead, DevMngt, 0, 0, 0, 0, 4, 0, 0xa0, 0x80, 0, 0})
	m.send(booted,
		kbdPacket(input.ModLShift, 0x0b), kbdPacket(0), // H
		kbdPacket(0, 0x0c), kbdPacket(0), // i
		kbdPacket(0, 0x28), kbdPacket(0)) // Enter
	if got := readKeys(t, keys, 3); got != "Hi\r" {
		t.Fatalf("typed %q", got)
	}

	m.mu.Lock()
	spien := append([]bool{}, m.spien...)
	m.mu.Unlock()
	if len(spien) != 3 || !spien[0] || spien[1] || !spien[2] {
		t.Fatalf("spien sequence %v, want 1 0 1", spien)
	}
	if v := m.pm.Get(0x208); v&pmgr.PS_ACTUAL != 0xf0 {
		t.Fatalf("ps_spi3 not on: %#x", v)
	}
	if v := m.ap.Get(4 * 52); (v&gpio.PERIPH)>>5 != 1 {
		t.Fatalf("pin 52 not muxed: %#x", v)
	}
	info := ""
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if info = strings.Join(svc.(dev.Lister).Devices(), "; "); strings.Contains(info, "7 packets") {
			break // the last key release has been read too
		}
	}
	for _, want := range []string{"ready pin", "8 power domains (7 switched on)", "4 pins muxed", "8 MHz",
		"7 packets (6 keyboard, crc ok 6)", "3 keys", "booted packets 1"} {
		if !strings.Contains(info, want) {
			t.Fatalf("lsdev info %q lacks %q", info, want)
		}
	}
}

func TestBlindFallback(t *testing.T) {
	old := readyTimeout
	readyTimeout = 50 * time.Millisecond
	defer func() { readyTimeout = old }()

	m := newMachine(false) // the ready line never moves
	cfg, err := ConfigFromFDT(t6000Tree(t).Find("/soc/spi@39b10c000/hid-transport@0"))
	if err != nil {
		t.Fatal(err)
	}
	keys := make(chan byte, 64)
	cfg.Keys = keys
	mgr, cancel := startKeyboard(t, m, cfg)
	defer cancel()
	svc, err := mgr.Get(context.Background(), "hid-transport@0")
	if err != nil {
		t.Fatal(err)
	}
	m.send(kbdPacket(0, 0x0a), kbdPacket(0)) // g
	if got := readKeys(t, keys, 1); got != "g" {
		t.Fatalf("typed %q", got)
	}
	if info := strings.Join(svc.(dev.Lister).Devices(), "; "); !strings.Contains(info, "blind reads (ready pin never asserted)") ||
		!strings.Contains(info, "empty") {
		t.Fatalf("lsdev info %q", info)
	}
}

func TestConfigError(t *testing.T) {
	m := newMachine(true)
	mgr, cancel := startKeyboard(t, m, errors.New("spihid: no spien-gpios"))
	defer cancel()
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if _, err := mgr.Get(ctx, "hid-transport@0"); err == nil || !strings.Contains(err.Error(), "no spien-gpios") {
		t.Fatalf("err = %v", err)
	}
}
