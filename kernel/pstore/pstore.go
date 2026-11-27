// Package pstore keeps the console's last output in memory that survives
// a reset (like Linux's ramoops): RAM the board sets aside, outside the Go
// heap, which QEMU keeps across system_reset and which a watchdog reset
// usually leaves alone. The next boot reads what the previous one printed
// last, and whether it ended with a clean halt; if not, that's the crash
// report (panic text, fault screen, the last log lines before a hang).
//
// Writes don't allocate or lock, so the board can call Write from the
// runtime's print hook (panics) on any core. Concurrent writers may
// interleave bytes; none are lost or written out of bounds.
package pstore

import (
	"encoding/binary"
	"sync/atomic"
)

// Layout of the region: a 32-byte header, then the ring.
//
//	0  magic "GDPSTORE"
//	8  ring size
//	12 bytes ever written this boot (mod 2^32)
//	16 flags (flagClean: the boot ended with a clean halt)
//	20 boot counter
const (
	headerSize = 32
	magic      = "GDPSTORE"
	flagClean  = 1
	// MinSize is the smallest region Open accepts.
	MinSize = headerSize + 256
)

// Ring is this boot's console record.
type Ring struct {
	hdr  []byte
	data []byte
	head atomic.Uint32
	busy atomic.Bool // one writer at a time updates the header's count

	prev      []byte
	prevClean bool
	prevValid bool
	boots     uint32
}

// Open takes the region, saves what the previous boot left in it, and
// starts this boot's record. mem must stay mapped and unused by anything
// else for the whole boot. nil if mem is too small.
func Open(mem []byte) *Ring {
	if len(mem) < MinSize {
		return nil
	}
	r := &Ring{hdr: mem[:headerSize], data: mem[headerSize:]}
	size := uint32(len(r.data))
	if string(r.hdr[:8]) == magic && binary.LittleEndian.Uint32(r.hdr[8:]) == size {
		r.prevValid = true
		r.prevClean = binary.LittleEndian.Uint32(r.hdr[16:])&flagClean != 0
		r.boots = binary.LittleEndian.Uint32(r.hdr[20:])
		head := binary.LittleEndian.Uint32(r.hdr[12:])
		if head <= size {
			r.prev = append([]byte(nil), r.data[:head]...)
		} else {
			at := head % size
			r.prev = append(append([]byte(nil), r.data[at:]...), r.data[:at]...)
		}
	}
	copy(r.hdr[:8], magic)
	binary.LittleEndian.PutUint32(r.hdr[8:], size)
	binary.LittleEndian.PutUint32(r.hdr[12:], 0)
	binary.LittleEndian.PutUint32(r.hdr[16:], 0)
	r.boots++
	binary.LittleEndian.PutUint32(r.hdr[20:], r.boots)
	return r
}

// Write records p. It never fails, allocates or blocks.
func (r *Ring) Write(p []byte) (int, error) {
	if r == nil || len(p) == 0 {
		return len(p), nil
	}
	n := uint32(len(p))
	size := uint32(len(r.data))
	pos := r.head.Add(n) - n
	for i := uint32(0); i < n; i++ {
		r.data[(pos+i)%size] = p[i]
	}
	r.publish()
	return len(p), nil
}

// WriteByte records one byte (the runtime's print hook).
func (r *Ring) WriteByte(c byte) error {
	if r == nil {
		return nil
	}
	size := uint32(len(r.data))
	pos := r.head.Add(1) - 1
	r.data[pos%size] = c
	r.publish()
	return nil
}

// publish stores the byte count in the header without blocking: a writer
// that finds another one publishing leaves it to that one, which looks
// again after letting go (so the last count is always stored).
func (r *Ring) publish() {
	for r.busy.CompareAndSwap(false, true) {
		h := r.head.Load()
		binary.LittleEndian.PutUint32(r.hdr[12:], h)
		r.busy.Store(false)
		if r.head.Load() == h {
			return
		}
	}
}

// MarkClean records that this boot is ending with a clean halt: the next
// boot won't report it as a crash.
func (r *Ring) MarkClean() {
	if r != nil {
		for !r.busy.CompareAndSwap(false, true) {
		}
		binary.LittleEndian.PutUint32(r.hdr[16:], flagClean)
		r.busy.Store(false)
	}
}

// Previous returns what the previous boot printed last, and whether that
// boot ended cleanly. ok is false when the region held no record (first
// boot after power-on, or the memory was overwritten).
func (r *Ring) Previous() (out []byte, clean, ok bool) {
	if r == nil {
		return nil, false, false
	}
	return r.prev, r.prevClean, r.prevValid
}

// Boots counts the boots recorded in this region (1: the first).
func (r *Ring) Boots() uint32 {
	if r == nil {
		return 0
	}
	return r.boots
}

// Crash returns the previous boot's last output if it ended without a
// clean halt (nil otherwise).
func (r *Ring) Crash() []byte {
	if out, clean, ok := r.Previous(); ok && !clean && len(out) > 0 {
		return out
	}
	return nil
}
