package wdt

import (
	"context"
	"testing"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
)

func TestArmPetStopReboot(t *testing.T) {
	f := mmio.NewFake()
	w := New(f)
	w.Arm(30 * time.Second)
	want := []mmio.Access{
		{Write: true, Off: CTRL, Val: 0},
		{Write: true, Off: CUR_TIME, Val: 0},
		{Write: true, Off: BITE_TIME, Val: 30 * ClockHz},
		{Write: true, Off: CTRL, Val: CTRL_RESET_EN},
	}
	if got := f.Log(); !equal(got, want) {
		t.Fatalf("Arm: %v", got)
	}
	f.Set(CUR_TIME, 12*ClockHz)
	if s := w.Devices()[0]; s != "armed, 30s timeout, bites in 18s" {
		t.Fatalf("Devices: %q", s)
	}
	f.Log()
	w.Pet()
	w.Stop()
	if got := f.Log(); !equal(got, []mmio.Access{{Write: true, Off: CUR_TIME}, {Write: true, Off: CTRL}}) {
		t.Fatalf("Pet/Stop: %v", got)
	}
	if s := w.Devices()[0]; s != "stopped" {
		t.Fatalf("Devices: %q", s)
	}
	f.Log()
	w.Reboot()
	if got := mmio.Writes(f.Log()); !equal(got, []mmio.Access{
		{Write: true, Off: CTRL, Val: CTRL_RESET_EN},
		{Write: true, Off: BITE_TIME, Val: 0},
		{Write: true, Off: CUR_TIME, Val: 0},
	}) {
		t.Fatalf("Reboot: %v", got)
	}
}

func TestTimeoutClamped(t *testing.T) {
	f := mmio.NewFake()
	New(f).Arm(time.Hour)
	if got := f.Get(BITE_TIME); got < 0xffff_0000 {
		t.Fatalf("BITE_TIME %#x: not clamped to the register", got)
	}
}

// The driver arms the watchdog, keeps petting it, and stops it when killed.
func TestDriverPetsAndStops(t *testing.T) {
	f := mmio.NewFake()
	pets := make(chan struct{}, 100)
	f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		if off == CUR_TIME && v == 0 && f.Get(CTRL) == CTRL_RESET_EN {
			select {
			case pets <- struct{}{}:
			default:
			}
		}
		return false
	}
	var armed *Watchdog
	for _, d := range Drivers(func(uint64) mmio.Regs { return f }, 30*time.Millisecond, func(w *Watchdog) { armed = w }) {
		dev.Register(d)
	}
	tab := proc.NewTable()
	m := dev.NewManager(tab, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx, []dev.Node{{Name: "watchdog", Compatible: "apple,t6000-wdt", Reg: []dev.Range{{Base: 0x2922b0000, Size: 0x4000}}}})
	svc, err := m.Get(ctx, "watchdog")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.(dev.Rebooter); !ok || armed == nil {
		t.Fatal("watchdog service is not a dev.Rebooter, or armed wasn't called")
	}
	for range 2 { // one from Arm, then at least one from the ticker
		select {
		case <-pets:
		case <-time.After(2 * time.Second):
			t.Fatal("watchdog not petted")
		}
	}
	cancel()
	grace, done := context.WithTimeout(context.Background(), 2*time.Second)
	defer done()
	tab.Shutdown(grace)
	if f.Get(CTRL) != 0 {
		t.Fatal("watchdog still armed after the driver stopped")
	}
}

func equal(a, b []mmio.Access) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
