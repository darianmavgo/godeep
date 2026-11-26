package fwcfg

import (
	"encoding/binary"
	"math/bits"
	"testing"

	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

const dmaBase = 0x1_2000_0000

// model is QEMU's fw_cfg DMA interface: items by key, a file directory,
// and a ramfb item that records what the guest wrote.
type model struct {
	f     *mmio.Fake
	mem   []byte
	items map[uint16][]byte
	ramfb []byte
	high  uint32
}

func newModel() *model {
	m := &model{f: mmio.NewFake(), mem: make([]byte, 64<<20), items: map[uint16][]byte{}}
	dir := make([]byte, 4)
	add := func(name string, key uint16, size int) {
		e := make([]byte, 64)
		binary.BigEndian.PutUint32(e[0:], uint32(size))
		binary.BigEndian.PutUint16(e[4:], key)
		copy(e[8:], name)
		dir = append(dir, e...)
		binary.BigEndian.PutUint32(dir[0:], binary.BigEndian.Uint32(dir[0:])+1)
	}
	add("bootorder", 0x20, 0)
	add("etc/ramfb", 0x21, 28)
	m.items[KeyFileDir] = dir
	m.f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		switch off {
		case RegDMAHigh:
			m.high = bits.ReverseBytes32(v)
		case RegDMALow:
			m.run(uint64(m.high)<<32 | uint64(bits.ReverseBytes32(v)))
		default:
			return false
		}
		return true
	}
	return m
}

func (m *model) at(phys uint64, n int) []byte { return m.mem[phys-dmaBase:][:n] }

func (m *model) run(addr uint64) {
	be := binary.BigEndian
	req := m.at(addr, 16)
	ctl := bits.ReverseBytes32(dma.LoadWord(m.mem, int(addr-dmaBase)))
	n := int(be.Uint32(req[4:]))
	data := m.at(be.Uint64(req[8:]), n)
	key := uint16(ctl >> 16)
	status := uint32(0)
	switch {
	case ctl&CtlSelect == 0:
		status = CtlError
	case ctl&CtlRead != 0:
		copy(data, m.items[key])
	case ctl&CtlWrite != 0 && key == 0x21 && n == 28:
		m.ramfb = append([]byte(nil), data...)
	default:
		status = CtlError
	}
	dma.StoreWord(m.mem, int(addr-dmaBase), bits.ReverseBytes32(status))
}

func TestRAMFB(t *testing.T) {
	m := newModel()
	a := dma.New(dma.Region{Base: dmaBase, Mem: m.mem})
	f, err := New(m.f, a)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	files, err := f.Files()
	if err != nil || len(files) != 2 || files[1] != (File{Name: "etc/ramfb", Key: 0x21, Size: 28}) {
		t.Fatalf("Files: %+v %v", files, err)
	}
	fb, err := f.SetupRAMFB(a, 640, 480)
	if err != nil {
		t.Fatal(err)
	}
	be := binary.BigEndian
	if m.ramfb == nil {
		t.Fatal("ramfb config never written")
	}
	if addr := be.Uint64(m.ramfb[0:]); addr < dmaBase || addr%4096 != 0 {
		t.Fatalf("framebuffer at %#x", addr)
	}
	if be.Uint32(m.ramfb[8:]) != FourCCXRGB8888 || be.Uint32(m.ramfb[16:]) != 640 ||
		be.Uint32(m.ramfb[20:]) != 480 || be.Uint32(m.ramfb[24:]) != 640*4 {
		t.Fatalf("config %x", m.ramfb)
	}
	if len(fb.Pix) != 640*480*4 || fb.Stride != 640*4 {
		t.Fatalf("framebuffer %dx%d stride %d, %d bytes", fb.Width, fb.Height, fb.Stride, len(fb.Pix))
	}
}

func TestNoRAMFB(t *testing.T) {
	m := newModel()
	m.items[KeyFileDir] = []byte{0, 0, 0, 0}
	a := dma.New(dma.Region{Base: dmaBase, Mem: m.mem})
	f, _ := New(m.f, a)
	defer f.Close()
	if _, err := f.SetupRAMFB(a, 640, 480); err == nil {
		t.Fatal("ramfb set up without the device")
	}
}

func TestDeviceError(t *testing.T) {
	m := newModel()
	a := dma.New(dma.Region{Base: dmaBase, Mem: m.mem})
	f, _ := New(m.f, a)
	defer f.Close()
	if err := f.Write(0x20, []byte{1}); err == nil {
		t.Fatal("write to a read-only item succeeded")
	}
}
