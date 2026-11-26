package dart

import (
	"encoding/binary"
	"testing"

	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

const base = 0x101_4000_0000

// walk translates iova through the tables like the DART does.
func walk(t *testing.T, mem []byte, ttbr uint32, iova uint64) uint64 {
	t.Helper()
	read := func(pa uint64, i uint64) uint64 {
		return binary.LittleEndian.Uint64(mem[pa-base+8*i:])
	}
	l1 := uint64(ttbr&^TTBR_VALID) << 12
	e1 := read(l1, iova>>25&2047)
	if e1&PTE_VALID == 0 {
		t.Fatalf("no L1 entry for %#x", iova)
	}
	e2 := read((e1>>10&(1<<30-1))<<14, iova>>14&2047)
	if e2&PTE_VALID == 0 {
		t.Fatalf("no L2 entry for %#x", iova)
	}
	return (e2>>10&(1<<30-1))<<14 | iova&(PageSize-1)
}

func TestLinearMap(t *testing.T) {
	mem := make([]byte, 64<<20)
	a := dma.New(dma.Region{Base: base, Mem: mem})
	const phys, size, bus = base + 0x80_0000, 40 << 20, 0x1000_0000 // spans two L2 tables
	tb, err := Build(a, phys, size, bus)
	if err != nil {
		t.Fatal(err)
	}
	f := mmio.NewFake()
	if err := Attach(f, 1, tb); err != nil {
		t.Fatal(err)
	}
	ttbr := f.Get(TTBR0 + 16)
	if ttbr&TTBR_VALID == 0 || f.Get(TCR0+4) != TCR_TRANSLATE || f.Get(ENABLED_STREAMS) != 2 {
		t.Fatalf("registers: ttbr %#x tcr %#x streams %#x", ttbr, f.Get(TCR0+4), f.Get(ENABLED_STREAMS))
	}
	for _, off := range []uint64{0, 0x3ff8, PageSize, 32<<20 - 8, 32 << 20, size - 1} {
		if got := walk(t, mem, ttbr, bus+off); got != phys+off {
			t.Fatalf("bus %#x -> %#x, want %#x", bus+off, got, phys+off)
		}
	}
	if len(tb.l2) != 2 {
		t.Fatalf("%d L2 tables", len(tb.l2))
	}
	tb.Free()
	if s := a.Stats(); s.Used != 0 {
		t.Fatalf("tables not freed: %+v", s)
	}
}

func TestLockedAndFault(t *testing.T) {
	a := dma.New(dma.Region{Base: base, Mem: make([]byte, 1<<20)})
	tb, err := Build(a, base, PageSize, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := mmio.NewFake()
	f.Set(CONFIG, CONFIG_LOCK)
	if Attach(f, 0, tb) == nil {
		t.Fatal("attached to a locked DART")
	}
	f.Set(ERROR, ERROR_FLAG|2<<24|4)
	f.Set(ERROR_ADDR_LO, 0x1234)
	if got := Fault(f); got != "fault 0x4 (stream 2) at 0x1234" {
		t.Fatalf("Fault = %q", got)
	}
}
