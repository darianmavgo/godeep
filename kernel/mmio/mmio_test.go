package mmio

import "testing"

func TestFakeRecordsAndModels(t *testing.T) {
	f := NewFake()
	// model a write-1-to-clear status register at 0x10
	f.OnWrite = func(f *Fake, off uint64, v uint32) bool {
		if off == 0x10 {
			f.Set(0x10, f.Get(0x10)&^v)
			return true
		}
		return false
	}
	f.Set(0x10, 0b1010)
	f.Write32(0x10, 0b0010) // clear bit 1 only
	if got := f.Read32(0x10); got != 0b1000 {
		t.Fatalf("W1C model: %#b", got)
	}
	Modify(f, 0x20, 0, 1<<3)
	log := f.Log()
	want := []Access{{true, 0x10, 2}, {false, 0x10, 8}, {false, 0x20, 0}, {true, 0x20, 8}}
	if len(log) != len(want) {
		t.Fatalf("log: %v", log)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("access %d: got %v want %v", i, log[i], want[i])
		}
	}
	if len(Writes(log)) != 2 || len(f.Log()) != 0 {
		t.Fatal("Writes/Log reset")
	}
}
