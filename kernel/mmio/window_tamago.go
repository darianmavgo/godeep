//go:build tamago

package mmio

import (
	"sync/atomic"
	"unsafe"
)

// Window is a real MMIO window at a physical (identity-mapped) address.
// Bare metal only: on a host OS these addresses aren't mapped.
type Window struct{ Base uint64 }

func (w Window) p(off uint64) *uint32 { return (*uint32)(unsafe.Pointer(uintptr(w.Base + off))) }

func (w Window) Read32(off uint64) uint32     { return atomic.LoadUint32(w.p(off)) }
func (w Window) Write32(off uint64, v uint32) { atomic.StoreUint32(w.p(off), v) }

// Read8 is a single byte load (for registers narrower than 32 bits, like
// the end of a virtio device's configuration).
func (w Window) Read8(off uint64) uint8 { return *(*uint8)(unsafe.Pointer(uintptr(w.Base + off))) }

// Store8 is a single byte store to a device register (NarrowRegs). Not
// for shared memory: see Write8.
func (w Window) Store8(off uint64, v uint8) { *(*uint8)(unsafe.Pointer(uintptr(w.Base + off))) = v }

// Read16 and Write16 are single 16-bit accesses (NarrowRegs); off must be
// 2-aligned.
func (w Window) Read16(off uint64) uint16 { return *(*uint16)(unsafe.Pointer(uintptr(w.Base + off))) }
func (w Window) Write16(off uint64, v uint16) {
	*(*uint16)(unsafe.Pointer(uintptr(w.Base + off))) = v
}

// Write8 stores one byte without disturbing its neighbours, even when
// another agent (a coprocessor) writes them at the same time: a
// compare-and-swap loop on the aligned 32-bit word that holds it. Shared
// memory only (lock flags), not device registers.
func (w Window) Write8(off uint64, v uint8) {
	a := w.Base + off
	p := (*uint32)(unsafe.Pointer(uintptr(a &^ 3)))
	shift := 8 * (a & 3)
	for {
		old := atomic.LoadUint32(p)
		if atomic.CompareAndSwapUint32(p, old, old&^(0xff<<shift)|uint32(v)<<shift) {
			return
		}
	}
}

// Read64 and Write64 are single 64-bit accesses, for registers that must be
// written whole (Apple's ASC mailbox: the second word of a message sends it).
func (w Window) Read64(off uint64) uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Pointer(uintptr(w.Base + off))))
}
func (w Window) Write64(off uint64, v uint64) {
	atomic.StoreUint64((*uint64)(unsafe.Pointer(uintptr(w.Base+off))), v)
}
