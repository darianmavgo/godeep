// Package mmio is how drivers touch device registers.
//
// Drivers take a Regs, never raw pointers: on hardware it's a Window (single
// 32-bit atomic accesses, which hypervisors such as HVF and m1n1 can decode),
// in host tests it's a Fake that models the device and records every access,
// so register sequences can be asserted or compared against traces captured
// on the real machine (docs/TargetMyMac.md §7.2, rung 2).
package mmio

import (
	"fmt"
	"sync"
)

// Regs is a device's register window.
type Regs interface {
	Read32(off uint64) uint32
	Write32(off uint64, v uint32)
}

// ByteRegs is a register window that also allows single-byte reads (Window
// and Fake do). Use it only where 32-bit reads don't work.
type ByteRegs interface {
	Regs
	Read8(off uint64) uint8
}

// Regs64 is a register window that also allows single 64-bit accesses
// (Window and Fake do).
type Regs64 interface {
	Regs
	Read64(off uint64) uint64
	Write64(off uint64, v uint64)
}

// Shared is memory shared with a coprocessor (Apple's GPU firmware): 32-
// and 64-bit words plus single bytes, each access atomic on its own
// (Window and Fake).
type Shared interface {
	Regs64
	Read8(off uint64) uint8
	Write8(off uint64, v uint8)
}

// Modify is a read-modify-write for ordinary read/write registers only.
//
// Never use it on write-1-to-set / write-1-to-clear registers (interrupt
// enable/pending banks, AIC masks): writing back the bits you read changes
// every other line in the bank. Write the single bit instead.
func Modify(r Regs, off uint64, clear, set uint32) {
	r.Write32(off, r.Read32(off)&^clear|set)
}

// Access is one recorded register access.
type Access struct {
	Write bool
	Off   uint64
	Val   uint32
}

func (a Access) String() string {
	if a.Write {
		return fmt.Sprintf("W %#05x <- %#x", a.Off, a.Val)
	}
	return fmt.Sprintf("R %#05x -> %#x", a.Off, a.Val)
}

// Fake is a register file for tests. Unset registers read as 0. OnRead and
// OnWrite let a test model device behaviour (status bits, W1C registers).
type Fake struct {
	mu      sync.Mutex
	regs    map[uint64]uint32
	log     []Access
	OnRead  func(f *Fake, off uint64) (v uint32, handled bool)
	OnWrite func(f *Fake, off uint64, v uint32) (handled bool)
}

func NewFake() *Fake { return &Fake{regs: map[uint64]uint32{}} }

func (f *Fake) Read32(off uint64) uint32 {
	v, ok := uint32(0), false
	if f.OnRead != nil {
		v, ok = f.OnRead(f, off)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !ok {
		v = f.regs[off]
	}
	f.log = append(f.log, Access{Off: off, Val: v})
	return v
}

func (f *Fake) Write32(off uint64, v uint32) {
	handled := false
	if f.OnWrite != nil {
		handled = f.OnWrite(f, off, v)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !handled {
		f.regs[off] = v
	}
	f.log = append(f.log, Access{Write: true, Off: off, Val: v})
}

// Read8 reads one byte of the 32-bit register that holds off.
func (f *Fake) Read8(off uint64) uint8 {
	return uint8(f.Read32(off&^3) >> (8 * (off & 3)))
}

// Write8 replaces one byte of the 32-bit register that holds off, under
// the Fake's lock (so a concurrent model's writes to the other bytes
// survive, as with Window's compare-and-swap). Logged as a 32-bit write.
func (f *Fake) Write8(off uint64, v uint8) {
	f.mu.Lock()
	defer f.mu.Unlock()
	shift := 8 * (off & 3)
	w := f.regs[off&^3]&^(0xff<<shift) | uint32(v)<<shift
	f.regs[off&^3] = w
	f.log = append(f.log, Access{Write: true, Off: off &^ 3, Val: w})
}

// Read64 reads two 32-bit registers, low word first (each logged; OnRead
// sees both).
func (f *Fake) Read64(off uint64) uint64 {
	lo := f.Read32(off)
	return uint64(lo) | uint64(f.Read32(off+4))<<32
}

// Write64 writes the low word, then the high word: a model triggers on
// the high word's write.
func (f *Fake) Write64(off uint64, v uint64) {
	f.Write32(off, uint32(v))
	f.Write32(off+4, uint32(v>>32))
}

// Set stores a register value without logging (device-side state).
func (f *Fake) Set(off uint64, v uint32) { f.mu.Lock(); f.regs[off] = v; f.mu.Unlock() }

// Get returns a register value without logging.
func (f *Fake) Get(off uint64) uint32 { f.mu.Lock(); defer f.mu.Unlock(); return f.regs[off] }

// Log returns the recorded accesses and clears the log.
func (f *Fake) Log() []Access {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.log
	f.log = nil
	return l
}

// Writes filters a log to its writes.
func Writes(l []Access) []Access {
	var out []Access
	for _, a := range l {
		if a.Write {
			out = append(out, a)
		}
	}
	return out
}
