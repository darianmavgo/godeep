package pgtable

import "testing"

// mem is sparse physical memory for tests.
type mem map[uint64]uint64

func (m mem) Read64(a uint64) uint64     { return m[a] }
func (m mem) Write64(a uint64, v uint64) { m[a] = v }

type check struct {
	va    uint64
	attr  Attr
	level int
	fault bool
}

func run(t *testing.T, b *Builder, checks []check) {
	t.Helper()
	root, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		pa, a, lvl, ok := Walk(b.Mem, root, c.va)
		if c.fault {
			if ok {
				t.Errorf("%#x: mapped, want fault", c.va)
			}
			continue
		}
		if !ok || pa != c.va || a != c.attr || (c.level != 0 && lvl != c.level) {
			t.Errorf("%#x: pa %#x attr %d level %d ok %v; want identity, attr %d level %d", c.va, pa, a, lvl, ok, c.attr, c.level)
		}
	}
}

// Apple M1 Pro: MMIO low, 16 GB RAM at 1 TB; godeep's RAM, text and DMA
// region inside it; framebuffer near the top left as device memory.
func TestAppleM1Layout(t *testing.T) {
	const dram = 0x100_0000_0000
	ram := uint64(dram + 4<<30) // godeep RAM: DRAM base + 4 GB
	text := ram + 0x100000      // kernel linked 1 MB in (arena below it)
	textEnd := text + 0x0c8000  // code + rodata, 4 KB aligned
	heapEnd := ram + 6<<30      // 6 GB of Go heap
	dmaEnd := heapEnd + 512<<20 // 512 MB DMA region
	b := &Builder{Mem: mem{}, ArenaNext: ram + 0x4000, ArenaEnd: text, Limit: dram + 16<<30}
	must(t, b.Add(0, NilGuard, Unmapped))
	must(t, b.Add(dram, ram, NormalNC)) // firmware/m1n1 RAM below godeep
	must(t, b.Add(ram, textEnd, NormalExec))
	must(t, b.Add(textEnd, heapEnd, Normal))
	must(t, b.Add(heapEnd, dmaEnd, Normal))
	must(t, b.Add(dmaEnd, dram+16<<30, NormalNC)) // framebuffer and firmware RAM above
	run(t, b, []check{
		{va: 0, fault: true},                            // nil
		{va: NilGuard - 8, fault: true},                 // nil + offset
		{va: NilGuard, attr: Device, level: 3},          // right after the guard
		{va: 0x2_3510_0000, attr: Device, level: 1},     // SoC MMIO (first 512 GB, 1 GB block)
		{va: dram, attr: NormalNC, level: 1},            // iBoot/m1n1 RAM below godeep: not ours
		{va: ram, attr: NormalExec, level: 3},           // vectors
		{va: text + 0x1234, attr: NormalExec, level: 3}, // kernel code
		{va: textEnd, attr: Normal, level: 3},           // first page after text: XN
		{va: ram + 3<<20, attr: Normal, level: 2},       // 2 MB blocks inside the first GB
		{va: ram + 2<<30, attr: Normal, level: 1},       // whole GB of heap: 1 GB block
		{va: heapEnd + 5, attr: Normal, level: 2},       // DMA region
		{va: dmaEnd, attr: NormalNC, level: 2},          // above: non-cacheable (framebuffer lives up here)
		{va: dram + 16<<30 - 8, attr: NormalNC},         // last byte of DRAM
		{va: dram + 16<<30, attr: Device},               // past DRAM
		{va: 0x200_0000_0000, fault: true},              // beyond Limit (rounded to 512 GB): faults
	})
	if used := b.ArenaNext - (ram + 0x4000); used > 16*4096 {
		t.Fatalf("used %d tables", used/4096)
	}
}

// QEMU virt: RAM at 1 GB, kernel 64 KB in: the same code that runs on the M1.
func TestQEMUVirtLayout(t *testing.T) {
	const ram = 0x4000_0000
	text, textEnd := uint64(ram+0xf000), uint64(ram+0xd8000)
	b := &Builder{Mem: mem{}, ArenaNext: ram + 0x4000, ArenaEnd: text, Limit: 1 << 39}
	must(t, b.Add(0, NilGuard, Unmapped))
	must(t, b.Add(ram, textEnd, NormalExec))
	must(t, b.Add(textEnd, ram+0xe000_0000, Normal))
	must(t, b.Add(0x1_2000_0000, 0x1_4000_0000, Normal)) // DMA region
	run(t, b, []check{
		{va: 0, fault: true}, // nil (QEMU's flash is there)
		{va: 0x1000, fault: true},
		{va: NilGuard, attr: Device},
		{va: 0x0900_0000, attr: Device}, // PL011
		{va: ram, attr: NormalExec, level: 3},
		{va: textEnd + 0x1000, attr: Normal, level: 3},
		{va: 0x1_2000_0000, attr: Normal},
		{va: 0x1_4000_0000, attr: Device},
		{va: 0x40_1000_0000, attr: Device}, // high PCIe window
	})
	if used := (b.ArenaNext - (ram + 0x4000)) / 4096; used > (text-ram-0x4000)/4096 {
		t.Fatalf("arena overflow: %d tables", used)
	} else {
		t.Logf("%d of %d table pages", used, (text-ram-0x4000)/4096)
	}
}

func TestErrors(t *testing.T) {
	b := &Builder{Mem: mem{}, ArenaNext: 0x10000, ArenaEnd: 0x11000, Limit: 1 << 39}
	if b.Add(0x1001, 0x2000, Normal) != ErrAlign {
		t.Fatal("unaligned region accepted")
	}
	must(t, b.Add(0x100000, 0x200000, Normal))
	if b.Add(0x150000, 0x300000, Normal) != ErrOrder {
		t.Fatal("overlap accepted")
	}
	if _, err := b.Build(); err != ErrArena { // one table of arena: L0 only
		t.Fatalf("want ErrArena, got %v", err)
	}
	if allocs := testing.AllocsPerRun(10, func() {
		b := &Builder{Mem: fixedMem{}, ArenaNext: 0x10000, ArenaEnd: 0x20000, Limit: 1 << 39}
		b.Add(0x40000000, 0x40200000, NormalExec)
		b.Build()
	}); allocs != 0 {
		t.Fatalf("Build allocates (%v allocs/run): unusable before the Go runtime starts", allocs)
	}
}

type fixedMem struct{}

func (fixedMem) Read64(uint64) uint64   { return 0 }
func (fixedMem) Write64(uint64, uint64) {}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
