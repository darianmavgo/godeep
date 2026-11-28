package net

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"godeep/drivers/pci"
	vmmio "godeep/drivers/virtio/mmio"
	"godeep/drivers/virtio/pci/legacytest"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
)

// legacyModel is Parallels' virtio-net: a legacy virtio-pci function whose
// header has no num_buffers field (10 bytes).
type legacyModel struct {
	*legacytest.Device
	mu   sync.Mutex
	sent [][]byte
}

func newLegacyModel() *legacyModel {
	mac := []byte{0x00, 0x1c, 0x42, 0xee, 0xc3, 0x9d, 1, 0} // MAC + status (link up)
	m := &legacyModel{Device: legacytest.New(FeatureMAC|FeatureStatus|1<<0, mac, make([]byte, 8<<20), dmaBase)}
	m.OnNotify = func(d *legacytest.Device, q int) {
		if q != 1 {
			return
		}
		for {
			b, ok := d.Pop(1)
			if !ok {
				break
			}
			f := append([]byte(nil), d.At(b.Addr, b.Len)[legacyHdrSize:]...)
			m.mu.Lock()
			m.sent = append(m.sent, f)
			m.mu.Unlock()
			d.Push(1, b.ID, 0)
		}
	}
	return m
}

func (m *legacyModel) Inject(frame []byte) bool {
	m.Lock()
	defer m.Unlock()
	b, ok := m.Pop(0)
	if ok {
		buf := m.At(b.Addr, b.Len)
		clear(buf[:legacyHdrSize])
		copy(buf[legacyHdrSize:], frame)
		m.Push(0, b.ID, legacyHdrSize+len(frame))
	}
	return ok
}

func (m *legacyModel) Sent() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.sent...)
}

func startLegacy(t *testing.T, m *legacyModel, f *pci.Function) (*dev.Manager, context.CancelFunc) {
	t.Helper()
	dev.Register(PCIDriver(func(uint64) mmio.Regs { return m.Device }, Options{Poll: time.Millisecond}))
	mgr := dev.NewManager(proc.NewTable(), nil, dma.New(dma.Region{Base: dmaBase, Mem: m.Mem}))
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx, []dev.Node{{Name: "net0", Compatible: PCICompatible, Config: f}})
	return mgr, cancel
}

func virtioNetFunction() *pci.Function {
	f := &pci.Function{Vendor: 0x1af4, Device: 0x1000, Class: 0x020000}
	f.BARs[0] = pci.BAR{CPU: 0x1000_6000, Size: 0x1000}
	return f
}

func TestLegacyPCI(t *testing.T) {
	m := newLegacyModel()
	mgr, cancel := startLegacy(t, m, virtioNetFunction())
	defer cancel()
	gctx, gcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer gcancel()
	svc, err := mgr.Get(gctx, "net0")
	if err != nil {
		t.Fatal(err)
	}
	nic := svc.(*NIC)
	if got := nic.MAC(); got != [6]byte{0x00, 0x1c, 0x42, 0xee, 0xc3, 0x9d} {
		t.Fatalf("MAC %x", got)
	}
	if n := m.Negotiated(); n != FeatureMAC|FeatureStatus {
		t.Fatalf("features %#x, want MAC|STATUS (not CSUM)", n)
	}
	if m.Status()&vmmio.StatusDriverOK == 0 {
		t.Fatalf("status %#x: no DRIVER_OK", m.Status())
	}
	ctx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()

	// More frames than the ring holds, both ways: buffers must recycle.
	for i := 0; i < 2*legacytest.QueueNum; i++ {
		if err := nic.Send(ctx, frame(60+i%1400, byte(i))); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	sent := m.Sent()
	if len(sent) != 2*legacytest.QueueNum {
		t.Fatalf("device saw %d frames", len(sent))
	}
	for i, f := range sent {
		if !bytes.Equal(f, frame(60+i%1400, byte(i))) {
			t.Fatalf("frame %d corrupted", i)
		}
	}
	for i := 0; i < 2*legacytest.QueueNum; i++ {
		want := frame(64+i%1450, byte(i*7))
		deadline := time.Now().Add(2 * time.Second)
		for !m.Inject(want) {
			if time.Now().After(deadline) {
				t.Fatalf("frame %d: no receive buffer", i)
			}
			time.Sleep(time.Millisecond)
		}
		got, err := nic.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("received frame %d corrupted (%d bytes)", i, len(got))
		}
	}
	if e := m.Errors(); len(e) > 0 {
		t.Fatalf("register accesses the device would mishandle: %v", e)
	}
}

func TestLegacyPCIRefusesOthers(t *testing.T) {
	for _, f := range []*pci.Function{
		{Vendor: 0x8086, Device: 0x100e, Class: 0x020000}, // e1000
		{Vendor: 0x1af4, Device: 0x1041, Class: 0x020000}, // modern virtio-net
	} {
		m := newLegacyModel()
		mgr, cancel := startLegacy(t, m, f)
		deadline := time.Now().Add(2 * time.Second)
		for {
			var st dev.Status
			for _, s := range mgr.List() {
				if s.Name == "net0" {
					st = s
				}
			}
			if st.Err != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%04x:%04x: driver didn't refuse it (%+v)", f.Vendor, f.Device, st)
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}
}
