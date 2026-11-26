package dma

import (
	"fmt"
	"sync/atomic"
	"unsafe"
)

// Device-shared words. A device writes DMA memory behind the CPU's back, so
// a driver polling it (an NVMe completion's phase bit, a descriptor's
// "done" flag) must load with acquire ordering: a plain load could be
// hoisted out of the loop, or the CPU could read the rest of the entry
// before the flag that says it's valid. Likewise a driver publishing a
// command stores the field that hands it over (or rings the doorbell after
// a Store32) with release ordering, so the device never sees it before the
// rest of the entry. On arm64 these are LDAR/STLR.
//
// This is the only unsafe code outside board/* and kernel/dev: the
// alternative is every driver casting slices itself.

func word(mem []byte, off int) *uint32 {
	if off < 0 || off%4 != 0 || off+4 > len(mem) {
		panic(fmt.Sprintf("dma: 32-bit access at %d in a %d-byte buffer", off, len(mem)))
	}
	return (*uint32)(unsafe.Pointer(&mem[off]))
}

// Load32 reads the little-endian word at byte offset off (4-aligned) with
// acquire ordering.
func (b *Buffer) Load32(off int) uint32 { return LoadWord(b.Bytes, off) }

// Store32 writes the word at off (4-aligned) with release ordering: every
// earlier write to DMA memory is visible first.
func (b *Buffer) Store32(off int, v uint32) { StoreWord(b.Bytes, off, v) }

// LoadWord and StoreWord are Load32 and Store32 on any slice of DMA memory.
// Device models in host tests use them for their side of a shared word (a
// ring index the driver publishes, a completion the model posts), which is
// what a real device's accesses amount to.
func LoadWord(mem []byte, off int) uint32 { return atomic.LoadUint32(word(mem, off)) }

func StoreWord(mem []byte, off int, v uint32) { atomic.StoreUint32(word(mem, off), v) }
