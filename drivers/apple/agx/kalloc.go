package agx

import (
	"fmt"
	"sync"

	"godeep/kernel/dma"
)

// Class is a kind of kernel GPU memory: a range of the kernel VM with
// its protection (Asahi: gpu.rs IOVA_KERN_* and KernelAllocators).
type Class int

const (
	Private   Class = iota // firmware-private, cached on the firmware side
	GPURO                  // the GPU reads it, the firmware writes it
	Shared                 // firmware ↔ kernel, uncached
	SharedRO               // the firmware only reads it
	GPUShared              // firmware ↔ GPU ↔ kernel
	RTKitBuf               // RTKit's buffers (crash log, syslog, ioreport)
	numClasses
)

// The CPU writes all of this memory through godeep's non-cacheable
// mapping of DRAM, which bypasses the coherency that lets the firmware
// cache it on Linux (Asahi maps Private and GPURO cached). So on godeep the
// firmware maps them uncached too: slower, never stale. Revisit once the
// DMA region is mapped cacheable (docs/AGX.md, open questions).
var classes = [numClasses]struct {
	name   string
	lo, hi uint64
	prot   Prot
}{
	Private:   {"private", 0xffffffa000000000, 0xffffffa600000000, ProtFWPrivRW.with(attrUncached)},
	GPURO:     {"gpu-ro", 0xffffffa600000000, 0xffffffa800000000, ProtGPUROFWPrivRW.with(attrUncached)},
	Shared:    {"shared", 0xffffffa800000000, 0xffffffaa00000000, ProtFWSharedRW},
	SharedRO:  {"shared-ro", 0xffffffaa00000000, 0xffffffac00000000, ProtFWSharedRO},
	GPUShared: {"gpu-shared", 0xffffffac00000000, 0xffffffae00000000, ProtGPUFWSharedRW},
	RTKitBuf:  {"rtkit", 0xffffffae00000000, 0xffffffae10000000, ProtFWSharedRW},
}

// Object is kernel memory mapped for the firmware.
type Object struct {
	VA    uint64 // GPU (firmware) virtual address
	Buf   *dma.Buffer
	Class Class
}

// Bytes is the CPU's view.
func (o *Object) Bytes() []byte { return o.Buf.Bytes }

// KernelAlloc hands out kernel GPU memory: DMA-region pages mapped into
// the kernel VM. Stage 2 never frees: a freed page could still be in the
// GPU's TLB or the firmware's cache until the TLB invalidation and the
// firmware cache flush are ported (docs/AGX.md), and a bump allocator
// that never reuses an address can't hit either.
type KernelAlloc struct {
	mu       sync.Mutex
	pt       *PageTable
	dma      *dma.Allocator
	next     [numClasses]uint64
	used     [numClasses]int
	nextMMIO uint64
}

// The firmware's view of device registers (Asahi: IOVA_KERN_MMIO_RANGE).
const (
	mmioLo = 0xffffffaf00000000
	mmioHi = 0xffffffb000000000
)

// NewKernelAlloc allocates in pt (the kernel's TTB1 table).
func NewKernelAlloc(pt *PageTable, a *dma.Allocator) *KernelAlloc {
	k := &KernelAlloc{pt: pt, dma: a, nextMMIO: mmioLo}
	for c := range k.next {
		k.next[c] = classes[c].lo
	}
	return k
}

// Alloc returns size bytes (rounded up to 16 KB pages) of class c, zeroed.
func (k *KernelAlloc) Alloc(c Class, size int) (*Object, error) {
	if size <= 0 || c < 0 || c >= numClasses {
		return nil, fmt.Errorf("agx: bad allocation %d bytes of class %d", size, c)
	}
	size = (size + PageSize - 1) &^ (PageSize - 1)
	k.mu.Lock()
	defer k.mu.Unlock()
	va := k.next[c]
	if va+uint64(size) > classes[c].hi {
		return nil, fmt.Errorf("agx: %s range full", classes[c].name)
	}
	b, err := k.dma.Alloc(size, PageSize)
	if err != nil {
		return nil, err
	}
	if err := k.pt.Map(va, b.Phys, uint64(size), classes[c].prot); err != nil {
		b.Free()
		return nil, err
	}
	k.next[c] = va + uint64(size) + PageSize // a guard page between objects
	k.used[c] += size
	return &Object{VA: va, Buf: b, Class: c}, nil
}

// Used reports the bytes allocated per class, for lsdev.
func (k *KernelAlloc) Used() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	s := ""
	for c := range numClasses {
		if k.used[c] > 0 {
			s += fmt.Sprintf(" %s %dK", classes[c].name, k.used[c]/1024)
		}
	}
	if s == "" {
		return "none"
	}
	return s[1:]
}

// RTKitIOMMU maps RTKit's buffers into the kernel VM, for kernel/rtkit
// (a dma.IOMMU): the GPU coprocessor sees the kernel VM, not physical
// memory.
type RTKitIOMMU struct{ K *KernelAlloc }

// Map maps b into the RTKit range.
func (r RTKitIOMMU) Map(b *dma.Buffer, _ dma.Direction) (uint64, error) {
	k := r.K
	size := (uint64(b.Size()) + PageSize - 1) &^ (PageSize - 1)
	if b.Phys&(PageSize-1) != 0 {
		return 0, fmt.Errorf("agx: RTKit buffer at %#x isn't 16 KB aligned", b.Phys)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	va := k.next[RTKitBuf]
	if va+size > classes[RTKitBuf].hi {
		return 0, fmt.Errorf("agx: rtkit range full")
	}
	if err := k.pt.Map(va, b.Phys, size, classes[RTKitBuf].prot); err != nil {
		return 0, err
	}
	k.next[RTKitBuf] = va + size + PageSize
	k.used[RTKitBuf] += int(size)
	return va, nil
}

// Unmap removes the mapping; the address is never reused (see KernelAlloc).
func (r RTKitIOMMU) Unmap(iova uint64, size int) error {
	return r.K.pt.Unmap(iova, (uint64(size)+PageSize-1)&^(PageSize-1))
}

// MapMMIO maps size bytes of device registers at phys (page-aligned) for
// the firmware, read-only unless writable. It returns the address.
func (k *KernelAlloc) MapMMIO(phys, size uint64, writable bool) (uint64, error) {
	if (phys|size)&(PageSize-1) != 0 || size == 0 {
		return 0, fmt.Errorf("agx: unaligned MMIO mapping %#x+%#x", phys, size)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	va := k.nextMMIO
	if va+size > mmioHi {
		return 0, fmt.Errorf("agx: MMIO range full")
	}
	p := ProtFWMMIORO
	if writable {
		p = ProtFWMMIORW
	}
	if err := k.pt.Map(va, phys, size, p); err != nil {
		return 0, err
	}
	k.nextMMIO = va + size + PageSize
	return va, nil
}

// MapAt maps an object a second time, at a fixed address in pt (the
// buffer manager control block lives at two addresses).
func MapAt(pt *PageTable, o *Object, va uint64, p Prot) error {
	return pt.Map(va, o.Buf.Phys, uint64(len(o.Bytes())), p)
}
