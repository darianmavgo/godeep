package dwc3

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

const gmem = 0x101_4000_0000

type devModel struct {
	t      *testing.T
	f      *mmio.Fake
	mem    []byte
	x      *gadget
	starts map[int][]uint64 // endpoint -> TRB addresses started
}

func newDevModel(t *testing.T) *devModel {
	m := &devModel{t: t, f: mmio.NewFake(), mem: make([]byte, 1<<20), starts: map[int][]uint64{}}
	m.f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		switch {
		case off == DCTL:
			f.Set(off, v&^DCTL_CSFTRST) // reset completes at once
			return true
		case off == DGCMD:
			f.Set(off, v&^CMD_ACT)
			return true
		case off >= DEPCMD && off < DEPCMD+0x10*6 && (off-DEPCMD)%0x10 == 0:
			n := int(off-DEPCMD) / 0x10
			if v&0xf == cmdStartTransfer {
				trb := uint64(f.Get(DEPCMDPAR1+0x10*uint64(n))) | uint64(f.Get(DEPCMDPAR0+0x10*uint64(n)))<<32
				m.starts[n] = append(m.starts[n], trb)
			}
			f.Set(off, v&^CMD_ACT)
			return true
		}
		return false
	}
	return m
}

func (m *devModel) at(bus uint64, n int) []byte { return m.mem[bus-gmem:][:n] }

// post queues device-controller events and runs the driver's poll.
func (m *devModel) post(evs ...uint32) {
	off := m.x.evOff
	for _, e := range evs {
		binary.LittleEndian.PutUint32(m.x.evs.Bytes[off:], e)
		off = (off + 4) % eventBufSize
	}
	m.f.Set(GEVNTCOUNT, uint32(4*len(evs)))
	m.x.poll()
}

// last returns the newest TRB started on endpoint n: its buffer and length.
func (m *devModel) last(n int) ([]byte, int, uint32) {
	s := m.starts[n]
	if len(s) == 0 {
		m.t.Fatalf("nothing started on endpoint %d", n)
	}
	trb := m.at(s[len(s)-1], 16)
	buf := uint64(binary.LittleEndian.Uint32(trb[0:])) | uint64(binary.LittleEndian.Uint32(trb[4:]))<<32
	l := int(binary.LittleEndian.Uint32(trb[8:]))
	return m.at(buf, frameBuf), l, binary.LittleEndian.Uint32(trb[12:])
}

// complete finishes endpoint n's transfer with left bytes not moved.
func (m *devModel) complete(n, left int) {
	s := m.starts[n]
	binary.LittleEndian.PutUint32(m.at(s[len(s)-1]+8, 4), uint32(left))
	m.post(uint32(n)<<1 | evXferComplete<<6)
}

func (m *devModel) control(setup []byte) []byte {
	buf, _, ctl := m.last(pEP0Out)
	if ctl&(0x3f<<4) != trbSetup {
		m.t.Fatalf("ep0 OUT not waiting for a setup packet (ctl %#x)", ctl)
	}
	copy(buf, setup)
	m.complete(pEP0Out, 0)
	var data []byte
	if m.x.ep0State == sDataSend {
		m.post(pEP0In<<1 | evXferNotReady<<6)
		b, l, _ := m.last(pEP0In)
		data = append([]byte(nil), b[:l]...)
		m.complete(pEP0In, 0)
		m.post(pEP0Out<<1 | evXferNotReady<<6) // status stage
		m.complete(pEP0Out, 0)
	} else {
		m.post(pEP0In<<1 | evXferNotReady<<6)
		m.complete(pEP0In, 0)
	}
	return data
}

func TestGadgetECM(t *testing.T) {
	m := newDevModel(t)
	a := dma.New(dma.Region{Base: gmem, Mem: m.mem})
	g := newGadget("usb@702280000")
	m.x = &gadget{r: m.f, g: g}
	if err := m.x.init(a); err != nil {
		t.Fatal(err)
	}
	if m.f.Get(DCTL)&DCTL_RUN_STOP == 0 {
		t.Fatal("not running")
	}
	m.post(1 | devtConnectDone<<8)

	if d := m.control([]byte{0x80, 6, 0, 1, 0, 0, 64, 0}); !bytes.Equal(d, deviceDesc) {
		t.Fatalf("device descriptor % x", d)
	}
	m.control([]byte{0x00, 5, 7, 0, 0, 0, 0, 0})
	if m.f.Get(DCFG)&DCFG_ADDR_MASK != 7<<3 {
		t.Fatal("address not set")
	}
	cfg := m.control([]byte{0x80, 6, 0, 2, 0, 0, 0xff, 0})
	if len(cfg) != len(configDesc()) || cfg[4] != 2 {
		t.Fatalf("config descriptor %d bytes", len(cfg))
	}
	mac := m.control([]byte{0x80, 6, 3, 3, 9, 4, 64, 0})
	if len(mac) != 26 {
		t.Fatalf("MAC string % x", mac)
	}
	m.control([]byte{0x00, 9, 1, 0, 0, 0, 0, 0})
	if m.f.Get(DALEPENA)&(1<<pBulkIn|1<<pBulkOt) == 0 {
		t.Fatal("bulk endpoints not enabled")
	}
	m.control([]byte{0x01, 0x0b, 1, 0, 1, 0, 0, 0}) // SET_INTERFACE data alt 1
	if !g.linkUp.Load() {
		t.Fatal("link not up")
	}

	// host -> godeep
	frame := bytes.Repeat([]byte{0xab}, 60)
	buf, l, _ := m.last(pBulkOt)
	copy(buf, frame)
	m.complete(pBulkOt, l-len(frame))
	got, err := g.Recv(context.Background())
	if err != nil || !bytes.Equal(got, frame) {
		t.Fatalf("Recv %x %v", got, err)
	}
	// godeep -> host, including a frame that fills whole packets
	for _, n := range []int{100, 1024} {
		g.Send(context.Background(), bytes.Repeat([]byte{byte(n)}, n))
		m.post()
		b, l, _ := m.last(pBulkIn)
		if l != n && !(n%512 == 0 && l == n+1) {
			t.Fatalf("%d-byte frame sent as %d", n, l)
		}
		if b[0] != byte(n) {
			t.Fatal("wrong data")
		}
		m.complete(pBulkIn, 0)
	}
	if g.txN.Load() != 2 || g.rxN.Load() != 1 {
		t.Fatalf("counters rx %d tx %d", g.rxN.Load(), g.txN.Load())
	}
}
