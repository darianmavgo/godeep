package fpwm

import (
	"testing"
	"time"

	"godeep/kernel/mmio"
)

func TestSet(t *testing.T) {
	f := mmio.NewFake()
	b := New(f, 24_000_000, 40*time.Microsecond)
	b.Set(25)
	if f.Get(ON_CYCLES) != 240 || f.Get(OFF_CYCLES) != 720 || f.Get(CTRL) != CTRL_ENABLE|CTRL_OUTPUT_ENABLE|CTRL_UPDATE {
		t.Fatalf("on %d off %d ctrl %#x", f.Get(ON_CYCLES), f.Get(OFF_CYCLES), f.Get(CTRL))
	}
	if New(f, 24_000_000, 40*time.Microsecond).Get() != 25 {
		t.Fatal("state not read back")
	}
	b.Set(0)
	if f.Get(CTRL) != 0 {
		t.Fatal("not off")
	}
	b.Set(150)
	if b.Get() != 100 || f.Get(OFF_CYCLES) != 0 {
		t.Fatal("not clamped")
	}
}
