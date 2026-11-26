package net

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	vmmio "godeep/drivers/virtio/mmio"
	"godeep/drivers/virtio/mmio/virtiotest"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/irq"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
)

const dmaBase = 0x1_2000_0000

// netModel is a virtio-net device: transmitted frames are collected, and
// Inject delivers a frame into the next receive buffer.
type netModel struct {
	*virtiotest.Device
	mu   sync.Mutex
	sent [][]byte
}

func newNetModel() *netModel {
	mac := []byte{0x52, 0x54, 0x00, 0x12, 0x34, 0x56, 1, 0} // MAC + status (link up)
	m := &netModel{Device: virtiotest.New(DeviceID, FeatureMAC|FeatureStatus|1<<0, mac, make([]byte, 8<<20), dmaBase)}
	m.OnNotify = func(d *virtiotest.Device, q int) {
		if q != 1 {
			return
		}
		for {
			b, ok := d.Pop(1)
			if !ok {
				break
			}
			f := append([]byte(nil), d.At(b.Addr, b.Len)[hdrSize:]...)
			m.mu.Lock()
			m.sent = append(m.sent, f)
			m.mu.Unlock()
			d.Push(1, b.ID, 0)
		}
	}
	return m
}

// Inject delivers frame like the device would: header + frame into the next
// receive buffer, then an interrupt. false if the driver gave no buffer.
func (m *netModel) Inject(frame []byte) bool {
	m.Lock()
	b, ok := m.Pop(0)
	if ok {
		buf := m.At(b.Addr, b.Len)
		clear(buf[:hdrSize])
		copy(buf[hdrSize:], frame)
		m.Push(0, b.ID, hdrSize+len(frame))
	}
	m.Unlock()
	if ok {
		m.Interrupt()
	}
	return ok
}

func (m *netModel) Sent() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.sent...)
}

type gic struct{}

func (gic) Enable(int)  {}
func (gic) Disable(int) {}

func start(t *testing.T, m *netModel, opt Options, bridge *irq.Bridge) (*NIC, context.CancelFunc, *proc.Table) {
	t.Helper()
	dev.Register(Driver(func(uint64) mmio.Regs { return m.F }, opt))
	tab := proc.NewTable()
	mgr := dev.NewManager(tab, bridge, dma.New(dma.Region{Base: dmaBase, Mem: m.Mem}))
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx, []dev.Node{{Name: "net0", Compatible: vmmio.Compatible(DeviceID),
		Reg: []dev.Range{{Base: 0xa000000, Size: 0x200}}, IRQ: []int{48}}})
	gctx, gcancel := context.WithTimeout(ctx, 5*time.Second)
	defer gcancel()
	svc, err := mgr.Get(gctx, "net0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return svc.(*NIC), cancel, tab
}

func frame(n int, seed byte) []byte {
	f := make([]byte, n)
	for i := range f {
		f[i] = byte(i) + seed
	}
	return f
}

func exercise(t *testing.T, m *netModel, nic *NIC) {
	t.Helper()
	if got := nic.MAC(); got != [6]byte{0x52, 0x54, 0x00, 0x12, 0x34, 0x56} {
		t.Fatalf("MAC %x", got)
	}
	if m.Negotiated()&vmmio.FeatureVersion1 == 0 || m.Negotiated()&1 != 0 {
		t.Fatalf("features %#x: want VERSION_1, not CSUM", m.Negotiated())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Transmit more frames than the ring holds: completions must recycle.
	for i := 0; i < 3*queueSize; i++ {
		if err := nic.Send(ctx, frame(60+i%1400, byte(i))); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	sent := m.Sent()
	if len(sent) != 3*queueSize {
		t.Fatalf("device saw %d frames", len(sent))
	}
	for i, f := range sent {
		if !bytes.Equal(f, frame(60+i%1400, byte(i))) {
			t.Fatalf("frame %d corrupted", i)
		}
	}

	// Receive, again more than the ring holds.
	for i := 0; i < 3*queueSize; i++ {
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
}

func TestPolled(t *testing.T) {
	m := newNetModel()
	nic, cancel, _ := start(t, m, Options{Poll: time.Millisecond}, nil)
	defer cancel()
	exercise(t, m, nic)
}

func TestInterrupts(t *testing.T) {
	m := newNetModel()
	b := irq.NewBridge(gic{})
	m.OnInterrupt = func() { b.Dispatch(48) }
	nic, cancel, _ := start(t, m, Options{}, b)
	defer cancel()
	exercise(t, m, nic)
}

func TestKillUnblocksUsers(t *testing.T) {
	m := newNetModel()
	nic, cancel, _ := start(t, m, Options{Poll: time.Millisecond}, nil)
	errc := make(chan error, 1)
	go func() { _, err := nic.Recv(context.Background()); errc <- err }()
	cancel()
	select {
	case err := <-errc:
		if err != ErrDown {
			t.Fatalf("Recv after stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv still blocked after the driver stopped")
	}
	if err := nic.Send(context.Background(), frame(60, 0)); err != ErrDown {
		t.Fatalf("Send after stop: %v", err)
	}
}

func TestLegacyTransportRefused(t *testing.T) {
	m := newNetModel()
	m.F.Set(vmmio.Version, 1)
	if _, err := vmmio.Init(context.Background(), m.F, 0); err != vmmio.ErrLegacy {
		t.Fatalf("Init on a legacy transport: %v", err)
	}
}
