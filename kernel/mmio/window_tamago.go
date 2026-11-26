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
