package xhci

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"godeep/drivers/pci"
	"godeep/drivers/usb"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/input"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
)

const (
	dmaBase = 0x1_2000_0000
	capLen  = 0x20
	dbOff   = 0x2000
	rtOff   = 0x3000
)

// usbDev is a device on a root port.
type usbDev struct {
	device, config []byte
	reportDesc     []byte   // HID report descriptor (report protocol)
	reports        [][]byte // queued interrupt reports
	requests       []string // control requests seen
}

func hidDevice(product uint16, protocol byte, mps int) *usbDev {
	dd := []byte{18, usb.DescDevice, 0x00, 0x02, 0, 0, 0, 8, 0x27, 0x06, byte(product), byte(product >> 8), 0, 0, 1, 2, 3, 1}
	cfg := []byte{
		9, usb.DescConfig, 34, 0, 1, 1, 0, 0xa0, 50,
		9, usb.DescInterface, 0, 0, 1, usb.ClassHID, usb.SubclassBoot, protocol, 0,
		9, 0x21, 0x11, 0x01, 0, 1, 0x22, 63, 0, // HID descriptor (skipped)
		7, usb.DescEndpoint, 0x81, 3, byte(mps), 0, 10,
	}
	return &usbDev{device: dd, config: cfg}
}

// model is an xHCI controller with devices on its root ports.
type model struct {
	mu     sync.Mutex
	f      *mmio.Fake
	mem    []byte
	ports  map[int]*usbDev
	portsc map[int]uint32

	cmdDeq   uint64
	cmdCCS   uint32
	evBase   uint64
	evN      int
	evEnq    int
	evPCS    uint32
	slots    map[int]*mslot
	nextSlot int
}

type mslot struct {
	port  int
	rings map[int]*mring // by DCI
	out   uint64
}

type mring struct {
	deq     uint64
	ccs     uint32
	pending []uint64 // interrupt: queued normal TRBs
}

func (m *model) at(p uint64, n int) []byte { return m.mem[p-dmaBase:][:n] }
func (m *model) u32(p uint64) uint32       { return binary.LittleEndian.Uint32(m.at(p, 4)) }
func (m *model) u64(p uint64) uint64       { return binary.LittleEndian.Uint64(m.at(p, 8)) }

func newModel(devs map[int]*usbDev) *model {
	m := &model{f: mmio.NewFake(), mem: make([]byte, 16<<20), ports: devs, portsc: map[int]uint32{}, slots: map[int]*mslot{}}
	f := m.f
	f.Set(CAPLENGTH, capLen|0x0110<<16)
	f.Set(HCSPARAMS1, 8|1<<8|4<<24)
	f.Set(HCCPARAMS1, 1) // AC64, 32-byte contexts
	f.Set(DBOFF, dbOff)
	f.Set(RTSOFF, rtOff)
	f.Set(capLen+USBSTS, STS_HALT)
	for p := 1; p <= 4; p++ {
		m.portsc[p] = PORT_PP
		if devs[p] != nil {
			m.portsc[p] |= PORT_CCS | PORT_CSC
		}
	}
	f.OnRead = func(f *mmio.Fake, off uint64) (uint32, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if off >= capLen+PORTSC0 && off < capLen+PORTSC0+0x40 {
			return m.portsc[int(off-capLen-PORTSC0)/0x10+1], true
		}
		if off == capLen+USBSTS {
			m.deliver() // the driver polls: hand over queued reports
		}
		return 0, false
	}
	f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case off == capLen+USBCMD:
			sts := uint32(STS_HALT)
			if v&CMD_RUN != 0 {
				sts = 0
			}
			f.Set(capLen+USBSTS, sts)
			f.Set(off, v&^CMD_RESET)
			return true
		case off == capLen+CRCR+4:
			lo := f.Get(capLen + CRCR)
			m.cmdDeq, m.cmdCCS = uint64(lo&^0x3f)|uint64(v)<<32, lo&1
		case off == rtOff+0x20+ERSTBA+4:
			erst := uint64(f.Get(rtOff+0x20+ERSTBA)) | uint64(v)<<32
			m.evBase, m.evN, m.evEnq, m.evPCS = m.u64(erst), int(m.u32(erst+8)), 0, 1
		case off >= capLen+PORTSC0 && off < capLen+PORTSC0+0x40:
			p := int(off-capLen-PORTSC0)/0x10 + 1
			cur := m.portsc[p] &^ (v & portChanges) // write-1-to-clear
			if v&PORT_PED != 0 {
				panic("driver disabled a port by writing PED")
			}
			if v&PORT_PR != 0 && cur&PORT_CCS != 0 {
				cur |= PORT_PED | PORT_PRC | 1<<10 // full speed
			}
			m.portsc[p] = cur
			return true
		case off == dbOff:
			m.commands()
			return true
		case off > dbOff && off < dbOff+0x100:
			m.transfer(int(off-dbOff)/4, int(v))
			return true
		}
		return false
	}
	return m
}

func (m *model) event(param uint64, status, ctl uint32) {
	p := m.evBase + uint64(m.evEnq*16)
	b := m.at(p, 16)
	binary.LittleEndian.PutUint64(b, param)
	binary.LittleEndian.PutUint32(b[8:], status)
	binary.LittleEndian.PutUint32(b[12:], ctl&^1|m.evPCS)
	m.evEnq++
	if m.evEnq == m.evN {
		m.evEnq, m.evPCS = 0, m.evPCS^1
	}
}

// next returns the next TRB of a ring (following links), if any.
func (m *model) next(deq *uint64, ccs *uint32) (addr uint64, param uint64, status, ctl uint32, ok bool) {
	for {
		ctl = m.u32(*deq + 12)
		if ctl&1 != *ccs {
			return 0, 0, 0, 0, false
		}
		if ctl>>10&0x3f == trbLink {
			if ctl&trbTC != 0 {
				*ccs ^= 1
			}
			*deq = m.u64(*deq)
			continue
		}
		addr, param, status = *deq, m.u64(*deq), m.u32(*deq+8)
		*deq += 16
		return addr, param, status, ctl, true
	}
}

func (m *model) commands() {
	for {
		addr, param, _, ctl, ok := m.next(&m.cmdDeq, &m.cmdCCS)
		if !ok {
			return
		}
		slot := int(ctl >> 24)
		switch ctl >> 10 & 0x3f {
		case trbEnableSlot:
			m.nextSlot++
			slot = m.nextSlot
			m.slots[slot] = &mslot{rings: map[int]*mring{}}
		case trbAddress:
			s := m.slots[slot]
			sc := param + 32
			s.port = int(m.u32(sc+4) >> 16 & 0xff)
			ep0 := m.u64(param + 64 + 8)
			s.rings[1] = &mring{deq: ep0 &^ 0xf, ccs: uint32(ep0 & 1)}
			dcbaa := uint64(m.f.Get(capLen+DCBAAP)) | uint64(m.f.Get(capLen+DCBAAP+4))<<32
			s.out = m.u64(dcbaa + 8*uint64(slot))
			copy(m.at(s.out, 32), m.at(sc, 32))
		case trbConfigureEP:
			s := m.slots[slot]
			add := m.u32(param + 4)
			for dci := 2; dci < 32; dci++ {
				if add&(1<<dci) != 0 {
					p := m.u64(param + uint64(32*(dci+1)) + 8)
					s.rings[dci] = &mring{deq: p &^ 0xf, ccs: uint32(p & 1)}
				}
			}
			copy(m.at(s.out, 32), m.at(param+32, 32))
		}
		m.event(addr, ccSuccess<<24, trbCommandEv<<10|uint32(slot)<<24)
	}
}

func (m *model) transfer(slot, dci int) {
	s := m.slots[slot]
	r := s.rings[dci]
	d := m.ports[s.port]
	if dci != 1 {
		for {
			addr, _, _, _, ok := m.next(&r.deq, &r.ccs)
			if !ok {
				break
			}
			r.pending = append(r.pending, addr)
		}
		m.deliver()
		return
	}
	var setup uint64
	for {
		addr, param, status, ctl, ok := m.next(&r.deq, &r.ccs)
		if !ok {
			return
		}
		switch ctl >> 10 & 0x3f {
		case trbSetup:
			setup = param
			d.requests = append(d.requests, fmt.Sprintf("%02x/%02x/%04x", byte(setup), byte(setup>>8), uint16(setup>>16)))
		case trbData:
			want := int(status & 0x1ffff)
			var src []byte
			if byte(setup>>8) == usb.ReqGetDescriptor {
				switch byte(setup >> 24) {
				case usb.DescDevice:
					src = d.device
				case usb.DescConfig:
					src = d.config
				case usb.DescHIDReport:
					src = d.reportDesc
				}
			}
			n := copy(m.at(param, want), src)
			code := uint32(ccSuccess)
			if n < want {
				code = ccShort
			}
			if ctl&trbIOC != 0 || code == ccShort {
				m.event(addr, code<<24|uint32(want-n), trbTransferEv<<10|uint32(slot)<<24|1<<16)
			}
		case trbStatus:
			m.event(addr, ccSuccess<<24, trbTransferEv<<10|uint32(slot)<<24|1<<16)
		}
	}
}

// deliver completes pending interrupt transfers with queued reports.
func (m *model) deliver() {
	for slot, s := range m.slots {
		for dci, r := range s.rings {
			d := m.ports[s.port]
			for dci != 1 && len(r.pending) > 0 && len(d.reports) > 0 {
				trb := r.pending[0]
				r.pending = r.pending[1:]
				rep := d.reports[0]
				d.reports = d.reports[1:]
				want := int(m.u32(trb+8) & 0x1ffff)
				n := copy(m.at(m.u64(trb), want), rep)
				code := uint32(ccSuccess)
				if n < want {
					code = ccShort
				}
				m.event(trb, code<<24|uint32(want-n), trbTransferEv<<10|uint32(slot)<<24|uint32(dci)<<16)
			}
		}
	}
}

func (m *model) report(port int, r ...byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ports[port].reports = append(m.ports[port].reports, r)
}

func TestKeyboardAndMouse(t *testing.T) {
	kbd, mouse := hidDevice(0x0001, usb.ProtoKeyboard, 8), hidDevice(0x0002, usb.ProtoMouse, 4)
	m := newModel(map[int]*usbDev{1: kbd, 3: mouse})
	keys := make(chan byte, 64)
	dev.Register(Driver(func(uint64) mmio.Regs { return m.f }, keys))
	mgr := dev.NewManager(proc.NewTable(), nil, dma.New(dma.Region{Base: dmaBase, Mem: m.mem}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &pci.Function{Class: 0x0c0330}
	f.BARs[0] = pci.BAR{CPU: 0x10010000, Size: 0x4000}
	mgr.Start(ctx, []dev.Node{{Name: "xhci0", Compatible: Compatible, Config: f}})
	svc, err := mgr.Get(ctx, "xhci0")
	if err != nil {
		t.Fatal(err)
	}
	lister := svc.(interface{ Devices() []string })
	deadline := time.Now().Add(5 * time.Second)
	var srcs []input.Source
	for len(srcs) < 2 || !strings.Contains(strings.Join(lister.Devices(), ";"), "port 3") {
		if time.Now().After(deadline) {
			t.Fatalf("devices: %v", lister.Devices())
		}
		time.Sleep(time.Millisecond)
		srcs = input.All(svc)
	}
	info := strings.Join(lister.Devices(), "; ")
	for _, want := range []string{"xHCI 1.10, 4 ports", "port 1: 0627:0001, full speed, slot 1: HID boot keyboard", "port 3: 0627:0002, full speed, slot 2: HID boot mouse"} {
		if !strings.Contains(info, want) {
			t.Fatalf("lsdev %q lacks %q", info, want)
		}
	}
	m.mu.Lock()
	reqs := strings.Join(kbd.requests, " ")
	m.mu.Unlock()
	// device (8 then 18 bytes), config (9 then all), set configuration, set boot protocol, set idle
	if reqs != "80/06/0100 80/06/0100 80/06/0200 80/06/0200 00/09/0001 21/0b/0000 21/0a/0000" {
		t.Fatalf("keyboard requests: %s", reqs)
	}

	ev := srcs[1].Subscribe(ctx)
	// typing: shift+h, i, enter (each key released)
	for _, r := range [][]byte{{2, 0, 0x0b}, {}, {0, 0, 0x0c}, {}, {0, 0, 0x28}, {}} {
		m.report(1, append(r, make([]byte, 8-len(r))...)...)
	}
	got := ""
	for len(got) < 3 {
		select {
		case b := <-keys:
			got += string(b)
		case <-time.After(5 * time.Second):
			t.Fatalf("typed %q", got)
		}
	}
	if got != "Hi\r" {
		t.Fatalf("typed %q", got)
	}

	m.report(3, 1, 5, 0xfd, 0) // left down, right 5, up 3
	m.report(3, 0, 0, 0, 0xff) // left up, wheel toward the user
	var evs []string
	for len(evs) < 4 {
		select {
		case e := <-ev:
			evs = append(evs, e.String())
		case <-time.After(5 * time.Second):
			t.Fatalf("mouse events %v", evs)
		}
	}
	if s := strings.Join(evs, "; "); s != "button 0 down; motion dx=5 dy=-3; button 0 up; wheel dx=0 dy=-1" {
		t.Fatalf("mouse events: %s", s)
	}
}

func TestInterval(t *testing.T) {
	for _, c := range []struct {
		s    usb.Speed
		b    byte
		want int
	}{{usb.Full, 1, 3}, {usb.Full, 10, 6}, {usb.Low, 255, 10}, {usb.High, 4, 3}, {usb.Super, 1, 0}} {
		if got := interval(c.s, c.b); got != c.want {
			t.Errorf("interval(%v, %d) = %d, want %d", c.s, c.b, got, c.want)
		}
	}
}

// tabletDevice is QEMU's usb-tablet: HID, no boot subclass, an absolute
// pointer in report protocol.
func tabletDevice() *usbDev {
	desc := []byte{
		0x05, 0x01, 0x09, 0x02, 0xa1, 0x01, 0x09, 0x01, 0xa1, 0x00,
		0x05, 0x09, 0x19, 0x01, 0x29, 0x03, 0x15, 0x00, 0x25, 0x01, 0x95, 0x03, 0x75, 0x01, 0x81, 0x02,
		0x95, 0x01, 0x75, 0x05, 0x81, 0x01,
		0x05, 0x01, 0x09, 0x30, 0x09, 0x31, 0x15, 0x00, 0x26, 0xff, 0x7f, 0x75, 0x10, 0x95, 0x02, 0x81, 0x02,
		0xc0, 0xc0,
	}
	dd := []byte{18, usb.DescDevice, 0x00, 0x02, 0, 0, 0, 8, 0x27, 0x06, 0x03, 0, 0, 0, 1, 2, 3, 1}
	cfg := []byte{
		9, usb.DescConfig, 34, 0, 1, 1, 0, 0xa0, 50,
		9, usb.DescInterface, 0, 0, 1, usb.ClassHID, 0, 0, 0,
		9, usb.DescHID, 0x11, 0x01, 0, 1, usb.DescHIDReport, byte(len(desc)), 0,
		7, usb.DescEndpoint, 0x81, 3, 8, 0, 10,
	}
	return &usbDev{device: dd, config: cfg, reportDesc: desc}
}

func TestReportProtocolTablet(t *testing.T) {
	tab := tabletDevice()
	m := newModel(map[int]*usbDev{2: tab})
	dev.Register(Driver(func(uint64) mmio.Regs { return m.f }, nil))
	mgr := dev.NewManager(proc.NewTable(), nil, dma.New(dma.Region{Base: dmaBase, Mem: m.mem}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &pci.Function{Class: 0x0c0330}
	f.BARs[0] = pci.BAR{CPU: 0x10010000, Size: 0x4000}
	mgr.Start(ctx, []dev.Node{{Name: "xhci0", Compatible: Compatible, Config: f}})
	svc, err := mgr.Get(ctx, "xhci0")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var srcs []input.Source
	for len(srcs) < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("no input source: %v", svc.(interface{ Devices() []string }).Devices())
		}
		time.Sleep(time.Millisecond)
		srcs = input.All(svc)
	}
	if info := strings.Join(svc.(interface{ Devices() []string }).Devices(), "; "); !strings.Contains(info, "slot 1: HID tablet") {
		t.Fatalf("lsdev: %s", info)
	}
	m.mu.Lock()
	reqs := strings.Join(tab.requests, " ")
	m.mu.Unlock()
	// no SET_PROTOCOL (report protocol is the default); the report descriptor from the interface
	if !strings.Contains(reqs, "81/06/2200") || strings.Contains(reqs, "21/0b") {
		t.Fatalf("tablet requests: %s", reqs)
	}
	ev := srcs[0].Subscribe(ctx)
	m.report(2, 1, 0xff, 0x7f, 0, 0)
	var evs []string
	for len(evs) < 2 {
		select {
		case e := <-ev:
			evs = append(evs, e.String())
		case <-time.After(5 * time.Second):
			t.Fatalf("tablet events %v", evs)
		}
	}
	if s := strings.Join(evs, "; "); s != "absolute x=32767 y=0; button 0 down" {
		t.Fatalf("tablet events: %s", s)
	}
}
