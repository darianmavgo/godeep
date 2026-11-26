package asc_test

import (
	"context"
	"testing"
	"time"

	"godeep/drivers/apple/asc"
	"godeep/drivers/apple/asc/asctest"
	"godeep/kernel/mmio"
	"godeep/kernel/rtkit"
)

func TestRTKitOverMailbox(t *testing.T) {
	const mgmt = 0
	typ := func(v uint64) uint64 { return v >> 52 & 0xff }
	model := asctest.NewModel(func(m rtkit.Message) []rtkit.Message {
		switch typ(m.Data) {
		case 2: // HELLO reply: one EPMAP page, last, no system endpoints, app EP 0x20
			return []rtkit.Message{{EP: mgmt, Data: 8<<52 | 1<<51 | 1<<32 | 1}}
		case 8: // EPMAP reply: report power on
			return []rtkit.Message{{EP: mgmt, Data: 7<<52 | rtkit.PwrOn}}
		case 0xb: // AP power state: ack
			return []rtkit.Message{{EP: mgmt, Data: m.Data}}
		}
		return nil
	})
	model.Post(rtkit.Message{EP: mgmt, Data: 1<<52 | 12<<16 | 11}) // HELLO 11..12
	mb := asc.New(model.F)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go mb.Run(ctx, nil)
	r := rtkit.New("test", mb, nil, nil)
	go r.Run(ctx)
	if err := r.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if in := r.Info(); in.Version != 12 || in.IOP != rtkit.PwrOn || in.AP != rtkit.PwrOn {
		t.Fatalf("info %+v", in)
	}
}

func TestCPU(t *testing.T) {
	f := mmio.NewFake()
	f.Set(asc.CPU_CONTROL, 0x1)
	asc.StartCPU(f)
	if !asc.CPURunning(f) || f.Get(asc.CPU_CONTROL) != 0x11 {
		t.Fatalf("asc.CPU_CONTROL %#x", f.Get(asc.CPU_CONTROL))
	}
	asc.StopCPU(f)
	if asc.CPURunning(f) {
		t.Fatal("still running")
	}
}
