package sart

import (
	"testing"

	"godeep/kernel/mmio"
)

func TestAllowKeepsIBootEntries(t *testing.T) {
	f := mmio.NewFake()
	f.Set(CONFIG, Allow) // entry 0: iBoot's
	f.Set(PADDR, 0x1234)
	s := New(f)
	if s.Protected() != 1 {
		t.Fatalf("protected %d", s.Protected())
	}
	if err := s.Allow(0x101_4020_0000, 510<<20); err != nil {
		t.Fatal(err)
	}
	if f.Get(CONFIG+4) != Allow || f.Get(PADDR+4) != 0x101_4020_0000>>12 || f.Get(SIZE+4) != 510<<8 {
		t.Fatalf("entry 1: %#x %#x %#x", f.Get(CONFIG+4), f.Get(PADDR+4), f.Get(SIZE+4))
	}
	if s.Allow(0x1001, 4096) == nil {
		t.Fatal("unaligned range accepted")
	}
	s.Clear()
	if f.Get(CONFIG) != Allow || f.Get(PADDR) != 0x1234 || f.Get(CONFIG+4) != 0 {
		t.Fatal("Clear touched iBoot's entry or kept ours")
	}
	for i := 0; i < Entries-1; i++ {
		s.Allow(0x1000, 4096)
	}
	if s.Allow(0x1000, 4096) == nil {
		t.Fatal("17th entry accepted")
	}
}
