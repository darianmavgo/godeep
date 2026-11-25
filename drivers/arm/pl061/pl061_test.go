package pl061

import (
	"context"
	"sync"
	"testing"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/irq"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
)

type ctl struct{ sync.Mutex }

func (*ctl) Enable(int)  {}
func (*ctl) Disable(int) {}

func TestPowerButton(t *testing.T) {
	f := mmio.NewFake()
	f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		if off == IC { // W1C
			f.Set(MIS, f.Get(MIS)&^v)
			return true
		}
		return false
	}
	pressed := make(chan struct{})
	var once sync.Once
	dev.Register(Driver(func(uint64) mmio.Regs { return f }, 3, func() { once.Do(func() { close(pressed) }) }))

	b := irq.NewBridge(&ctl{})
	m := dev.NewManager(proc.NewTable(), b, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx, []dev.Node{{Name: "gpio", Compatible: "arm,pl061", Reg: []dev.Range{{Base: 0x9030000, Size: 0x1000}}, IRQ: []int{39}}})
	if _, err := m.Get(ctx, "gpio"); err != nil {
		t.Fatal(err)
	}
	// configuration: edge, single edge, rising, clear, enable, all for bit 3 only
	w := mmio.Writes(f.Log())
	if len(w) != 5 || w[3] != (mmio.Access{Write: true, Off: IC, Val: 1 << 3}) || f.Get(IE) != 1<<3 || f.Get(IEV) != 1<<3 {
		t.Fatalf("config writes: %v", w)
	}

	f.Set(MIS, 1<<3) // the button rises
	b.Dispatch(39)
	select {
	case <-pressed:
	case <-time.After(2 * time.Second):
		t.Fatal("power button not reported")
	}
	if f.Get(MIS) != 0 {
		t.Fatal("interrupt not cleared")
	}
}
