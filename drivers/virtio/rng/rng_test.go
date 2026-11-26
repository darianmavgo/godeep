package rng

import (
	"bytes"
	"context"
	"testing"
	"time"

	vmmio "godeep/drivers/virtio/mmio"
	"godeep/drivers/virtio/mmio/virtiotest"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
)

const dmaBase = 0x1_2000_0000

func TestSeedAndRead(t *testing.T) {
	m := virtiotest.New(DeviceID, 0, nil, make([]byte, 1<<20), dmaBase)
	var next byte
	m.OnNotify = func(d *virtiotest.Device, q int) {
		for {
			b, ok := d.Pop(0)
			if !ok {
				return
			}
			if !b.Write {
				t.Error("request buffer not device-writable")
			}
			n := min(b.Len, 48) // a device may return fewer bytes than asked
			for i, buf := 0, d.At(b.Addr, n); i < n; i++ {
				buf[i] = next
				next++
			}
			d.Push(0, b.ID, n)
		}
	}
	seeded := make(chan []byte, 1)
	dev.Register(Driver(func(uint64) mmio.Regs { return m.F }, Options{Seed: func(s []byte) { seeded <- append([]byte(nil), s...) }}))
	mgr := dev.NewManager(proc.NewTable(), nil, dma.New(dma.Region{Base: dmaBase, Mem: m.Mem}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mgr.Start(ctx, []dev.Node{{Name: "rng0", Compatible: vmmio.Compatible(DeviceID), Reg: []dev.Range{{Base: 0xa000000}}}})
	svc, err := mgr.Get(ctx, "rng0")
	if err != nil {
		t.Fatal(err)
	}
	seed := <-seeded
	want := make([]byte, SeedSize)
	for i := range want {
		want[i] = byte(i)
	}
	if !bytes.Equal(seed, want) {
		t.Fatalf("seed %x", seed)
	}
	r := svc.(*RNG)
	p := make([]byte, 100)
	n, err := r.Read(ctx, p)
	if err != nil || n != 48 || p[0] != SeedSize {
		t.Fatalf("Read: n=%d err=%v first=%d", n, err, p[0])
	}
	if r.Devices()[0] != "entropy from the host, seeded crypto/rand: 112 bytes read" {
		t.Fatalf("Devices: %q", r.Devices()[0])
	}
}
