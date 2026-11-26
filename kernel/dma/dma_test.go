package dma

import (
	"testing"
)

func newTest(size int) *Allocator {
	return New(Region{Base: 0x1_2000_0000, Mem: make([]byte, size)})
}

func TestAllocAlignZeroAndFree(t *testing.T) {
	a := newTest(1 << 20)
	b1, err := a.Alloc(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b1.Size() != 128 || b1.Phys%DefaultAlign != 0 {
		t.Fatalf("bad buffer: size %d phys %#x", b1.Size(), b1.Phys)
	}
	b1.Bytes[0] = 0xff
	b2, err := a.Alloc(5000, 16384)
	if err != nil {
		t.Fatal(err)
	}
	if b2.Phys%16384 != 0 {
		t.Fatalf("not 16K aligned: %#x", b2.Phys)
	}
	b1.Free()
	b3, _ := a.Alloc(64, 0) // reuses b1's space, must be zeroed
	if b3.Phys != b1.Phys || b3.Bytes[0] != 0 {
		t.Fatalf("reuse/zero failed: %#x %#x %d", b3.Phys, b1.Phys, b3.Bytes[0])
	}
	b2.Free()
	b3.Free()
	if s := a.Stats(); s.Used != 0 || s.FreeSpans != 1 || s.LargestFreeSpan != 1<<20 {
		t.Fatalf("not fully coalesced: %+v", s)
	}
}

func TestOutOfMemoryAndDoubleFree(t *testing.T) {
	a := newTest(4096)
	b, _ := a.Alloc(4096, 0)
	if _, err := a.Alloc(1, 0); err != ErrNoMemory {
		t.Fatalf("want ErrNoMemory, got %v", err)
	}
	b.Free()
	defer func() {
		if recover() == nil {
			t.Fatal("double free did not panic")
		}
	}()
	b.Free()
}

func TestFragmentationCoalesces(t *testing.T) {
	a := newTest(64 * 128)
	var bs []*Buffer
	for i := 0; i < 64; i++ {
		b, err := a.Alloc(128, 0)
		if err != nil {
			t.Fatal(err)
		}
		bs = append(bs, b)
	}
	for i := 0; i < 64; i += 2 { // free evens, then odds: worst-case fragmentation
		bs[i].Free()
	}
	if _, err := a.Alloc(256, 0); err != ErrNoMemory {
		t.Fatal("expected fragmentation to block a 256-byte alloc")
	}
	for i := 1; i < 64; i += 2 {
		bs[i].Free()
	}
	if s := a.Stats(); s.FreeSpans != 1 {
		t.Fatalf("want 1 span, got %+v", s)
	}
}

func TestLoadStore32(t *testing.T) {
	a := newTest(1 << 16)
	b, err := a.Alloc(64, 0)
	if err != nil {
		t.Fatal(err)
	}
	b.Store32(8, 0xdeadbeef)
	if b.Bytes[8] != 0xef || b.Bytes[11] != 0xde || b.Load32(8) != 0xdeadbeef {
		t.Fatal("not little-endian at the right offset")
	}
	for _, off := range []int{2, -4, len(b.Bytes)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("offset %d accepted", off)
				}
			}()
			b.Load32(off)
		}()
	}
}

func TestTranslatedView(t *testing.T) {
	a := newTest(1 << 20)
	v := a.Translated(0x8000_0000)
	if base, size := v.Region(); base != 0x1_2000_0000 || size != 1<<20 {
		t.Fatalf("Region = %#x, %d", base, size)
	}
	b1, err := a.Alloc(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b1.Bus != b1.Phys {
		t.Fatalf("untranslated Bus %#x != Phys %#x", b1.Bus, b1.Phys)
	}
	b2, err := v.Alloc(5000, 16384)
	if err != nil {
		t.Fatal(err)
	}
	if b2.Bus != b2.Phys-0x1_2000_0000+0x8000_0000 || b2.Bus%16384 != 0 {
		t.Fatalf("Bus %#x for Phys %#x", b2.Bus, b2.Phys)
	}
	if b2.Phys < b1.Phys+uint64(b1.Size()) && b1.Phys < b2.Phys+uint64(b2.Size()) {
		t.Fatal("the view and its allocator handed out overlapping memory")
	}
	if s := a.Stats(); s.Buffers != 2 || v.Stats() != s {
		t.Fatalf("stats not shared: %+v vs %+v", s, v.Stats())
	}
	b2.Free()
	b1.Free()
	if s := v.Stats(); s.Used != 0 || s.FreeSpans != 1 {
		t.Fatalf("not fully freed: %+v", s)
	}
}
