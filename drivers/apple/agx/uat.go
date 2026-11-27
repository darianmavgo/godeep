package agx

import (
	"fmt"

	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

// The UAT is the GPU's MMU (Asahi: pgtable.rs, mmu.rs). Its tables are
// close to arm64's: 16 KB pages, 3 levels of 2048 8-byte entries, 39-bit
// input addresses. Table entries and leaf entries both have type 3.
const (
	PageBits = 14
	PageSize = 1 << PageBits
	IAS      = 39 // input (GPU virtual) address bits
	levels   = 3
	lvBits   = PageBits - 3 // 2048 entries per table
	nPTE     = 1 << lvBits

	pteTypeMask  = 3
	pteTable     = 3 // table pointer, and leaf page
	pteNonGlobal = 1 << 11
	pteAF        = 1 << 10
	apShift      = 6
	attrShift    = 2
	highShift    = 52
	protMask     = 3<<apShift | 7<<attrShift | 0xfff<<highShift

	// Kernel (firmware) addresses: the top of the 64-bit space, on TTB1.
	KernBase = 0xffffffa000000000
	KernTop  = 0xffffffb000000000
	// User contexts and the firmware's lower kernel VM, on TTB0.
	UserBase = PageSize
	UserTop  = 1 << IAS

	// Slots in the TTBAT ("ttbs" region): 64 contexts of (ttb0, ttb1).
	NumCtx       = 64
	slotSize     = 16
	ttbrValid    = 1
	ttbrASIDBits = 48
)

// Memory attributes and access permissions of a page.
const (
	attrCached   = 0
	attrDevice   = 1
	attrUncached = 2

	apFWGPU = 0 // firmware and GPU
	apFW    = 1 // firmware only (GPU access via bits below)
	apGPU   = 2 // GPU only

	highPXN       = 1 << 1
	highUXN       = 1 << 2
	highGPUAccess = 1 << 3
)

// Prot is a page's protection: who may access it (firmware, GPU), how,
// and its memory type.
type Prot struct {
	ap, attr uint8
	high     uint16
}

func prot(ap, uxn, pxn uint8) Prot {
	return Prot{ap: ap, high: highGPUAccess | uint16(pxn)*highPXN | uint16(uxn)*highUXN}
}

func (p Prot) with(attr uint8) Prot { p.attr = attr; return p }

func (p Prot) pte() uint64 {
	return uint64(p.ap)<<apShift | uint64(p.high)<<highShift | uint64(p.attr)<<attrShift | pteAF
}

// Protections used by the driver (Asahi: pgtable::prot).
var (
	ProtFWMMIORW      = prot(apFW, 1, 0).with(attrDevice)
	ProtFWMMIORO      = prot(apFW, 0, 0).with(attrDevice)
	ProtFWSharedRW    = prot(apFW, 1, 0).with(attrUncached)
	ProtFWSharedRO    = prot(apFW, 0, 0).with(attrUncached)
	ProtFWPrivRW      = prot(apFW, 1, 0).with(attrCached)
	ProtGPUFWSharedRW = prot(apFWGPU, 1, 1).with(attrUncached)
	ProtGPUFWPrivRW   = prot(apFWGPU, 1, 1).with(attrCached)
	ProtGPUROFWPrivRW = prot(apFW, 1, 1).with(attrCached)
	ProtGPUSharedRW   = prot(apGPU, 1, 0).with(attrUncached)
	ProtGPUSharedRO   = prot(apGPU, 0, 0).with(attrUncached)
	ProtGPUSharedWO   = prot(apGPU, 0, 1).with(attrUncached)
)

// Pages allocates zeroed, page-aligned page-table pages.
type Pages interface {
	AllocPage() (phys uint64, err error)
	FreePage(phys uint64)
}

// PageTable is one UAT address space. Its pages are read and written
// through phys, a window whose offsets are physical addresses.
type PageTable struct {
	phys  mmio.Regs64
	pages Pages
	ttb   uint64
	owned bool // a user/lower table we allocated (non-global entries)
	oas   uint64
	lo    uint64 // the VA range it serves
	hi    uint64
}

// NewPageTable allocates an empty address space covering [lo, hi).
func NewPageTable(phys mmio.Regs64, pages Pages, oas uint, lo, hi uint64) (*PageTable, error) {
	ttb, err := pages.AllocPage()
	if err != nil {
		return nil, err
	}
	return &PageTable{phys: phys, pages: pages, ttb: ttb, owned: true, oas: 1<<oas - 1, lo: lo, hi: hi}, nil
}

// BorrowPageTable uses an existing top-level table: the kernel's, which
// iBoot built in the "pagetables" region with the firmware mapped in it.
// Its entries are global.
func BorrowPageTable(phys mmio.Regs64, pages Pages, ttb uint64, oas uint, lo, hi uint64) (*PageTable, error) {
	if ttb&(PageSize-1) != 0 || (lo|hi)&(PageSize-1) != 0 {
		return nil, fmt.Errorf("agx: unaligned page table %#x [%#x, %#x)", ttb, lo, hi)
	}
	return &PageTable{phys: phys, pages: pages, ttb: ttb, oas: 1<<oas - 1, lo: lo, hi: hi}, nil
}

// TTB is the physical address of the top-level table.
func (pt *PageTable) TTB() uint64 { return pt.ttb }

// leaf returns the physical address of the leaf entry for va, creating
// the intermediate tables when alloc is set (0: not mapped and !alloc).
func (pt *PageTable) leaf(va uint64, alloc bool) (uint64, error) {
	va &= 1<<IAS - 1
	table := pt.ttb
	for level := levels - 1; level > 0; level-- {
		idx := va >> (PageBits + level*lvBits) & (nPTE - 1)
		ea := table + idx*8
		e := pt.phys.Read64(ea)
		if e == 0 {
			if !alloc {
				return 0, nil
			}
			p, err := pt.pages.AllocPage()
			if err != nil {
				return 0, err
			}
			e = p | pteTable
			pt.phys.Write64(ea, e)
		} else if e&pteTypeMask != pteTable {
			return 0, fmt.Errorf("agx: unexpected table entry %#x at level %d for %#x", e, level, va)
		}
		table = e & pt.oas &^ (PageSize - 1)
	}
	return table + (va>>PageBits&(nPTE-1))*8, nil
}

func (pt *PageTable) check(va, size uint64) error {
	if (va|size)&(PageSize-1) != 0 || size == 0 {
		return fmt.Errorf("agx: unaligned mapping %#x+%#x", va, size)
	}
	if va < pt.lo || va+size > pt.hi || va+size < va {
		return fmt.Errorf("agx: %#x+%#x outside [%#x, %#x)", va, size, pt.lo, pt.hi)
	}
	return nil
}

// Map maps [va, va+size) to [pa, pa+size). It refuses to replace a
// mapping (a stale one is a bug that would let the GPU write anywhere).
func (pt *PageTable) Map(va, pa, size uint64, p Prot) error {
	if err := pt.check(va, size); err != nil {
		return err
	}
	if pa&(PageSize-1) != 0 || (pa+size-1)&^pt.oas != 0 {
		return fmt.Errorf("agx: physical %#x+%#x not mappable", pa, size)
	}
	bits := pteTable | p.pte()
	if pt.owned {
		bits |= pteNonGlobal
	}
	for off := uint64(0); off < size; off += PageSize {
		ea, err := pt.leaf(va+off, true)
		if err != nil {
			return err
		}
		if old := pt.phys.Read64(ea); old != 0 {
			return fmt.Errorf("agx: %#x already mapped (%#x)", va+off, old)
		}
		pt.phys.Write64(ea, pa+off|bits)
	}
	return nil
}

// Unmap removes the mappings of [va, va+size). Intermediate tables stay.
// The caller must invalidate the GPU's TLB before reusing the pages.
func (pt *PageTable) Unmap(va, size uint64) error {
	if err := pt.check(va, size); err != nil {
		return err
	}
	for off := uint64(0); off < size; off += PageSize {
		ea, err := pt.leaf(va+off, false)
		if err != nil {
			return err
		}
		if ea != 0 {
			pt.phys.Write64(ea, 0)
		}
	}
	return nil
}

// Lookup returns the physical address and entry va maps to.
func (pt *PageTable) Lookup(va uint64) (pa, pte uint64, ok bool) {
	ea, err := pt.leaf(va&^(PageSize-1), false)
	if err != nil || ea == 0 {
		return 0, 0, false
	}
	pte = pt.phys.Read64(ea)
	if pte&pteTypeMask != pteTable {
		return 0, pte, false
	}
	return pte&pt.oas&^(PageSize-1) | va&(PageSize-1), pte, true
}

// DMAPages allocates page-table pages from the DMA region (physically
// contiguous, outside the Go heap; the GPU walks them by physical address).
type DMAPages struct {
	A    *dma.Allocator
	bufs map[uint64]*dma.Buffer
}

func (d *DMAPages) AllocPage() (uint64, error) {
	b, err := d.A.Alloc(PageSize, PageSize)
	if err != nil {
		return 0, err
	}
	if d.bufs == nil {
		d.bufs = map[uint64]*dma.Buffer{}
	}
	d.bufs[b.Phys] = b
	return b.Phys, nil
}

func (d *DMAPages) FreePage(phys uint64) {
	if b, ok := d.bufs[phys]; ok {
		b.Free()
		delete(d.bufs, phys)
	}
}

// Count is the number of pages allocated.
func (d *DMAPages) Count() int { return len(d.bufs) }
