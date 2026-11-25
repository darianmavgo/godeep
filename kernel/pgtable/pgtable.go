// Package pgtable builds arm64 stage-1 translation tables: 4 KB granule,
// 48-bit virtual addresses (TCR_EL1.T0SZ = 16), four levels, identity
// mapped (VA == PA, as TamaGo expects).
//
// TamaGo's own MMU code starts at level 1 with a 39-bit address space, so it
// can only map the first 512 GB. Apple M1 Pro RAM starts at 0x100_0000_0000
// (1 TB), which needs a level-0 table: that's what this package adds.
//
// It runs before the Go runtime exists (TamaGo's Hwinit0), so it must not
// allocate: no slices that grow, no fmt, no interfaces holding values.
package pgtable

// Attr is what a range of memory is mapped as.
type Attr uint8

const (
	Device     Attr = iota // Device-nGnRnE, execute-never (MMIO; anything not listed)
	Normal                 // cacheable memory, execute-never (heap, DMA buffers)
	NormalExec             // cacheable memory, executable (kernel text, vectors)
	Unmapped               // invalid: faults on access
	NormalNC               // non-cacheable memory, execute-never (framebuffers, firmware RAM)
)

// Descriptor bits (ARM ARM D8.3). Attribute indexes match MAIR, see MAIR.
const (
	descValid = 1 << 0
	descTable = 0b11 // level 0-2 table, or level 3 page
	descBlock = 0b01 // level 1-2 block

	attrIdxShift = 2
	shInner      = 0b11 << 8
	shOuter      = 0b10 << 8
	af           = 1 << 10
	pxn          = 1 << 53
	uxn          = 1 << 54

	OutputMask = 0x0000_ffff_ffff_f000

	mairDevice   = 0 // index 0: Device-nGnRnE (0x00)
	mairNormal   = 1 // index 1: Normal, inner/outer write-back (0xff)
	mairNormalNC = 2 // index 2: Normal, non-cacheable (0x44)

	// MAIR is the MAIR_EL1 value these descriptors assume. The early boot
	// map in the boards' entry code uses the same value.
	MAIR = 0x44<<(8*mairNormalNC) | 0xff<<(8*mairNormal) | 0x00<<(8*mairDevice)
)

// TCR returns the TCR_EL1 value for these tables, given the CPU's physical
// address size field (ID_AA64MMFR0_EL1.PARange, bits 3:0).
func TCR(parange uint64) uint64 {
	if parange > 0b110 {
		parange = 0b101 // 48 bits
	}
	return parange<<32 | // IPS
		1<<23 | // EPD1: no TTBR1 walks
		0b00<<14 | // TG0: 4 KB
		0b11<<12 | // SH0: inner shareable
		0b01<<10 | // ORGN0: write-back
		0b01<<8 | // IRGN0: write-back
		16 // T0SZ: 48-bit VA
}

func leafBits(a Attr) uint64 {
	switch a {
	case Normal:
		return af | shInner | mairNormal<<attrIdxShift | pxn | uxn
	case NormalExec:
		return af | shInner | mairNormal<<attrIdxShift
	case NormalNC:
		return af | shOuter | mairNormalNC<<attrIdxShift | pxn | uxn
	default:
		return af | shOuter | mairDevice<<attrIdxShift | pxn | uxn
	}
}

// Memory is where tables are written (physical memory with the MMU off).
type Memory interface {
	Read64(addr uint64) uint64
	Write64(addr uint64, v uint64)
}

// Region is a mapped range; bounds must be 4 KB aligned.
type Region struct {
	Start, End uint64
	Attr       Attr
}

// MaxRegions is how many regions a Builder takes (fixed: no allocation).
const MaxRegions = 8

// Builder lays out tables in an arena of physical memory.
type Builder struct {
	Mem Memory
	// Arena for tables: [ArenaNext, ArenaEnd), 4 KB aligned.
	ArenaNext, ArenaEnd uint64
	// Limit: addresses [0, Limit) are mapped (Device unless a region says
	// otherwise); above it everything faults. Rounded up to 512 GB.
	Limit uint64
	// Regions, sorted by Start, non-overlapping.
	Regions [MaxRegions]Region
	N       int
}

// Error is a static error (no allocation).
type Error string

func (e Error) Error() string { return string(e) }

const (
	ErrArena     = Error("pgtable: out of table arena")
	ErrAlign     = Error("pgtable: region not 4 KB aligned")
	ErrOrder     = Error("pgtable: regions overlap or are unsorted")
	ErrTooMany   = Error("pgtable: too many regions")
	ErrLimit     = Error("pgtable: limit beyond 48-bit address space")
	ErrArenaSize = Error("pgtable: arena not 4 KB aligned")
)

// Add appends a region.
func (b *Builder) Add(start, end uint64, a Attr) error {
	if b.N == MaxRegions {
		return ErrTooMany
	}
	if start&0xfff != 0 || end&0xfff != 0 || end <= start {
		return ErrAlign
	}
	if b.N > 0 && start < b.Regions[b.N-1].End {
		return ErrOrder
	}
	b.Regions[b.N] = Region{start, end, a}
	b.N++
	return nil
}

// attrRange reports the attribute of [addr, end) if uniform; split=true if
// the range crosses a region boundary.
func (b *Builder) attrRange(addr, end uint64) (a Attr, split bool) {
	a = Device
	for i := 0; i < b.N; i++ {
		r := b.Regions[i]
		if end <= r.Start || addr >= r.End {
			continue // disjoint
		}
		if addr >= r.Start && end <= r.End {
			return r.Attr, false
		}
		return 0, true
	}
	return a, false
}

func (b *Builder) alloc() (uint64, error) {
	if b.ArenaNext+4096 > b.ArenaEnd {
		return 0, ErrArena
	}
	t := b.ArenaNext
	b.ArenaNext += 4096
	for i := uint64(0); i < 512; i++ {
		b.Mem.Write64(t+8*i, 0)
	}
	return t, nil
}

// Build writes the tables and returns the level-0 table address (TTBR0_EL1).
func (b *Builder) Build() (uint64, error) {
	if b.ArenaNext&0xfff != 0 || b.ArenaEnd&0xfff != 0 {
		return 0, ErrArenaSize
	}
	if b.Limit > 1<<48 {
		return 0, ErrLimit
	}
	l0, err := b.alloc()
	if err != nil {
		return 0, err
	}
	for i := uint64(0); i<<39 < b.Limit; i++ {
		l1, err := b.alloc()
		if err != nil {
			return 0, err
		}
		b.Mem.Write64(l0+8*i, l1|descTable)
		if err := b.fill(l1, i<<39, 1); err != nil {
			return 0, err
		}
	}
	return l0, nil
}

// fill populates the table at t covering base.. at level lvl (1, 2 or 3).
func (b *Builder) fill(t, base uint64, lvl int) error {
	shift := uint(39 - 9*lvl) // 30, 21, 12
	for i := uint64(0); i < 512; i++ {
		addr := base + i<<shift
		end := addr + 1<<shift
		a, split := b.attrRange(addr, end)
		if split && lvl == 3 {
			return ErrAlign // unreachable with 4 KB aligned regions
		}
		switch {
		case split:
			next, err := b.alloc()
			if err != nil {
				return err
			}
			b.Mem.Write64(t+8*i, next|descTable)
			if err := b.fill(next, addr, lvl+1); err != nil {
				return err
			}
		case a == Unmapped:
			b.Mem.Write64(t+8*i, 0)
		case lvl == 3:
			b.Mem.Write64(t+8*i, addr|leafBits(a)|descTable) // L3 page
		default:
			b.Mem.Write64(t+8*i, addr|leafBits(a)|descBlock)
		}
	}
	return nil
}

// Walk translates va through the tables at root like the MMU would, for
// tests and diagnostics. It returns the attribute, the level of the leaf
// (1 = 1 GB block, 2 = 2 MB block, 3 = 4 KB page) and ok=false on a fault.
func Walk(m Memory, root, va uint64) (pa uint64, a Attr, level int, ok bool) {
	t := root
	for lvl := 0; lvl <= 3; lvl++ {
		shift := uint(39 - 9*lvl)
		d := m.Read64(t + 8*((va>>shift)&511))
		if d&descValid == 0 {
			return 0, 0, lvl, false
		}
		leaf := lvl == 3 || d&0b11 == descBlock
		if !leaf {
			t = d & OutputMask
			continue
		}
		size := uint64(1) << shift
		pa = d&OutputMask&^(size-1) | va&(size-1)
		switch {
		case d>>attrIdxShift&7 == mairDevice:
			a = Device
		case d>>attrIdxShift&7 == mairNormalNC:
			a = NormalNC
		case d&pxn != 0:
			a = Normal
		default:
			a = NormalExec
		}
		return pa, a, lvl, true
	}
	return 0, 0, 3, false
}
