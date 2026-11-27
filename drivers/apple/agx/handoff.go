package agx

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"godeep/kernel/mmio"
)

// The handoff region (iBoot's "gfx-handoff", the tree's "uat-handoff") is
// how the kernel and the GPU firmware share the UAT: a two-party lock
// (Peterson's algorithm: a flag each plus a turn), the context the
// firmware is using, and per-context TLB flush requests (Asahi: mmu.rs
// struct Handoff).
const (
	hoMagicAP = 0x00 // u64
	hoMagicFW = 0x08 // u64
	hoLockAP  = 0x10 // u8
	hoLockFW  = 0x11 // u8
	hoTurn    = 0x14 // u32: 1 = the firmware's turn
	hoCurSlot = 0x18 // u32
	hoFlush   = 0x20 // [NumCtx+1] × {state, addr, size u64}
	hoUnk2    = hoFlush + (NumCtx+1)*24
	hoUnk3    = hoUnk2 + 8
	// HandoffSize is the size of the structure.
	HandoffSize = hoUnk3 + 8

	// PPLMagic is what both sides write to say they're there.
	PPLMagic = 0x4b1d000000000002

	// HandoffTimeout is how long Init waits for the firmware's magic.
	HandoffTimeout = time.Second
)

// ErrNoFirmware means the firmware never answered the handoff.
var ErrNoFirmware = errors.New("agx: handoff: no answer from the GPU firmware (not running?)")

// Handoff is the shared handoff region.
type Handoff struct{ m mmio.Shared }

// NewHandoff wraps the region at m.
func NewHandoff(m mmio.Shared) *Handoff { return &Handoff{m: m} }

// Lock takes the lock against the firmware. It spins (the firmware holds
// it for a few instructions), yielding the core: TamaGo can't preempt.
func (h *Handoff) Lock(ctx context.Context) error {
	h.m.Write8(hoLockAP, 1)
	for n := 0; h.m.Read8(hoLockFW) != 0; n++ {
		if h.m.Read32(hoTurn) != 0 { // the firmware's turn: step back
			h.m.Write8(hoLockAP, 0)
			for h.m.Read32(hoTurn) != 0 {
				if err := spin(ctx, &n); err != nil {
					return err
				}
			}
			h.m.Write8(hoLockAP, 1)
		}
		if err := spin(ctx, &n); err != nil {
			h.m.Write8(hoLockAP, 0)
			return err
		}
	}
	return nil
}

// Unlock gives the turn to the firmware and releases the lock.
func (h *Handoff) Unlock() {
	h.m.Write32(hoTurn, 1)
	h.m.Write8(hoLockAP, 0)
}

func spin(ctx context.Context, n *int) error {
	*n++
	if *n%64 == 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	runtime.Gosched()
	return nil
}

// CurrentSlot is the context the firmware has mapped for unprivileged
// access, if any.
func (h *Handoff) CurrentSlot() (uint32, bool) {
	s := h.m.Read32(hoCurSlot)
	return s, s != 0 && s != 0xffffffff
}

// Init announces the kernel and waits for the firmware's magic (Asahi:
// Handoff::init). The coprocessor must already be running.
func (h *Handoff) Init(ctx context.Context) error {
	h.m.Write64(hoMagicAP, PPLMagic)
	h.m.Write32(hoCurSlot, 0)
	h.m.Write64(hoUnk3, 0)
	deadline := time.Now().Add(HandoffTimeout)
	for {
		if err := h.Lock(ctx); err != nil {
			return err
		}
		ok := h.m.Read64(hoMagicFW) == PPLMagic
		h.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w (magic %#x)", ErrNoFirmware, h.m.Read64(hoMagicFW))
		}
		t := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	for i := uint64(0); i <= NumCtx; i++ {
		h.m.Write64(hoFlush+i*24, 0)
		h.m.Write64(hoFlush+i*24+8, 0)
		h.m.Write64(hoFlush+i*24+16, 0)
	}
	return nil
}

// TTBs is the TTBAT ("uat-ttbs"): the page tables of each context slot.
type TTBs struct{ m mmio.Regs64 }

// NewTTBs wraps the region at m.
func NewTTBs(m mmio.Regs64) *TTBs { return &TTBs{m: m} }

// SetKernel installs the kernel's tables in slot 0 and clears every other
// slot, under the handoff lock (Asahi: Uat::new).
func (t *TTBs) SetKernel(ctx context.Context, h *Handoff, ttb0, ttb1 uint64) error {
	if err := h.Lock(ctx); err != nil {
		return err
	}
	defer h.Unlock()
	t.m.Write64(0, ttb0|ttbrValid)
	t.m.Write64(8, ttb1|ttbrValid)
	for i := uint64(1); i < NumCtx; i++ {
		t.m.Write64(i*slotSize, 0)
		t.m.Write64(i*slotSize+8, 0)
	}
	return nil
}

// Slot returns a slot's (ttb0, ttb1).
func (t *TTBs) Slot(i int) (ttb0, ttb1 uint64) {
	return t.m.Read64(uint64(i) * slotSize), t.m.Read64(uint64(i)*slotSize + 8)
}
