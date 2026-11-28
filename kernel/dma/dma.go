// Package dma allocates memory that devices can read and write directly.
//
// DMA buffers live in a Region outside the Go heap: the garbage collector
// never scans, moves or frees them, so their physical address stays valid
// while a device uses them. A buffer is owned by exactly one party at a time;
// pass *Buffer over channels to hand it off, and Free it exactly once.
//
// Devices behind an IOMMU (Apple DART) see buffers at an I/O virtual address
// returned by IOMMU.Map; without one (QEMU virt), use Identity.
package dma

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// DefaultAlign is the minimum alignment and size granule: one cache line on
// Apple M1 (128 bytes), so buffers never share a line with other data.
const DefaultAlign = 128

// Region is DMA-capable memory: Mem is the CPU's view of the physical
// range starting at Base.
type Region struct {
	Base uint64
	Mem  []byte
}

// Buffer is an allocated DMA buffer.
type Buffer struct {
	Phys  uint64 // physical address (what an IOMMU maps, or the device sees directly)
	Bus   uint64 // the address to give the device: Phys, or the IOVA (Translated)
	Bytes []byte // CPU view of the buffer

	a     *Allocator
	off   int
	size  int
	freed bool
}

// Size is the allocated size (the request rounded up to DefaultAlign).
func (b *Buffer) Size() int { return b.size }

// Free returns the buffer to its allocator. It panics on double free: that
// would mean two owners, which is a bug that corrupts device memory.
func (b *Buffer) Free() {
	if b.a == nil {
		panic(fmt.Sprintf("dma: Free of a view at %#x: free the buffer it came from", b.Phys))
	}
	b.a.free(b)
}

// View is bytes [off, off+n) of b as a Buffer of its own that shares b's
// memory: for one allocation a device wants laid out in parts (a legacy
// virtqueue's rings, page-aligned within one block). Free b, never a view.
func (b *Buffer) View(off, n int) *Buffer {
	if off < 0 || n < 0 || off+n > len(b.Bytes) {
		panic(fmt.Sprintf("dma: view [%d, %d) of a %d-byte buffer", off, off+n, len(b.Bytes)))
	}
	return &Buffer{Phys: b.Phys + uint64(off), Bus: b.Bus + uint64(off), Bytes: b.Bytes[off : off+n : off+n], size: n}
}

type span struct{ off, size int }

// Allocator is a first-fit allocator over a Region with coalescing frees.
type Allocator struct {
	base uint64
	mem  []byte
	bus  uint64     // Bus - Phys (mod 2^64): 0 unless Translated
	core *Allocator // a Translated view's allocator (its spans and stats); nil: itself

	mu    sync.Mutex
	spans []span // free spans, sorted by off, never adjacent
	used  int
	peak  int
	count int
}

// ErrNoMemory is returned when no free span can satisfy a request.
var ErrNoMemory = errors.New("dma: out of memory")

// New returns an allocator managing r.
func New(r Region) *Allocator {
	return &Allocator{base: r.Base, mem: r.Mem, spans: []span{{0, len(r.Mem)}}}
}

// Translated returns a view of a's region for devices behind an IOMMU
// that maps the whole region linearly at busBase (an Apple DART set up
// that way): it allocates from the same memory, and its buffers' Bus is
// busBase plus their offset in the region.
func (a *Allocator) Translated(busBase uint64) *Allocator {
	c := a.root()
	return &Allocator{base: c.base, mem: c.mem, bus: busBase - c.base, core: c}
}

// Region returns the physical base and size of the memory a allocates from.
func (a *Allocator) Region() (base uint64, size int) {
	c := a.root()
	return c.base, len(c.mem)
}

func (a *Allocator) root() *Allocator {
	if a.core != nil {
		return a.core
	}
	return a
}

func alignUp(x, a uint64) uint64 { return (x + a - 1) &^ (a - 1) }

// Alloc returns a zeroed buffer of at least size bytes whose physical address
// is a multiple of align (a power of two; values below DefaultAlign are raised).
// Use align 16384 for buffers an Apple DART maps (16 KB pages).
func (a *Allocator) Alloc(size, align int) (*Buffer, error) {
	if size <= 0 {
		return nil, fmt.Errorf("dma: bad size %d", size)
	}
	if align < DefaultAlign {
		align = DefaultAlign
	}
	if align&(align-1) != 0 {
		return nil, fmt.Errorf("dma: alignment %d is not a power of two", align)
	}
	size = int(alignUp(uint64(size), DefaultAlign))

	view := a
	a = a.root()
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, s := range a.spans {
		start := int(alignUp(a.base+uint64(s.off), uint64(align)) - a.base)
		if start+size > s.off+s.size {
			continue
		}
		// carve [start, start+size) out of s
		var repl []span
		if start > s.off {
			repl = append(repl, span{s.off, start - s.off})
		}
		if end := start + size; end < s.off+s.size {
			repl = append(repl, span{end, s.off + s.size - end})
		}
		a.spans = append(a.spans[:i], append(repl, a.spans[i+1:]...)...)

		a.used += size
		a.count++
		if a.used > a.peak {
			a.peak = a.used
		}
		b := a.mem[start : start+size : start+size]
		clear(b)
		phys := a.base + uint64(start)
		return &Buffer{Phys: phys, Bus: phys + view.bus, Bytes: b, a: a, off: start, size: size}, nil
	}
	return nil, ErrNoMemory
}

func (a *Allocator) free(b *Buffer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b.freed {
		panic(fmt.Sprintf("dma: double free of buffer at %#x", b.Phys))
	}
	b.freed = true
	b.Bytes = nil
	a.used -= b.size
	a.count--

	i := sort.Search(len(a.spans), func(i int) bool { return a.spans[i].off > b.off })
	a.spans = append(a.spans, span{})
	copy(a.spans[i+1:], a.spans[i:])
	a.spans[i] = span{b.off, b.size}
	// coalesce with next, then previous
	if i+1 < len(a.spans) && a.spans[i].off+a.spans[i].size == a.spans[i+1].off {
		a.spans[i].size += a.spans[i+1].size
		a.spans = append(a.spans[:i+1], a.spans[i+2:]...)
	}
	if i > 0 && a.spans[i-1].off+a.spans[i-1].size == a.spans[i].off {
		a.spans[i-1].size += a.spans[i].size
		a.spans = append(a.spans[:i], a.spans[i+1:]...)
	}
}

// Stats describes allocator usage.
type Stats struct {
	Base            uint64
	Size            int
	Used, Peak      int
	Buffers         int
	FreeSpans       int
	LargestFreeSpan int
}

func (a *Allocator) Stats() Stats {
	a = a.root()
	a.mu.Lock()
	defer a.mu.Unlock()
	s := Stats{Base: a.base, Size: len(a.mem), Used: a.used, Peak: a.peak, Buffers: a.count, FreeSpans: len(a.spans)}
	for _, sp := range a.spans {
		s.LargestFreeSpan = max(s.LargestFreeSpan, sp.size)
	}
	return s
}

// Direction is the direction of a DMA transfer, for IOMMU permissions and
// cache maintenance.
type Direction int

const (
	ToDevice Direction = iota
	FromDevice
	Bidirectional
)

// IOMMU translates device addresses. Apple Silicon puts a DART in front of
// every DMA-capable device; drivers must Map a buffer and hand the device the
// returned I/O virtual address (IOVA), never Buffer.Phys.
type IOMMU interface {
	Map(b *Buffer, dir Direction) (iova uint64, err error)
	Unmap(iova uint64, size int) error
}

// Identity is the IOMMU of a machine without one (QEMU virt): IOVA == phys.
type Identity struct{}

func (Identity) Map(b *Buffer, _ Direction) (uint64, error) { return b.Phys, nil }
func (Identity) Unmap(uint64, int) error                    { return nil }
