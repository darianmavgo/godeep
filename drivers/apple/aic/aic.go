// Package aic drives the M1 Pro's interrupt controller (AIC2, "apple,
// t6000-aic"), after Linux's irq-apple-aic.c, as an irq.Controller.
//
// Reading the event register acknowledges the highest pending interrupt
// and masks its line; the driver unmasks it when done (irq.Line.Done),
// which is godeep's oneshot pattern already. Interrupt numbers here are
// die<<24 | irq, the event register's own encoding.
//
// The board polls Event from a kernel thread instead of taking IRQ
// exceptions (see board/applem1): interrupts still arrive as channel
// events, without changing the exception vectors of a boot path that
// works.
package aic

import (
	"errors"
	"fmt"

	"godeep/kernel/fdt"
	"godeep/kernel/mmio"
)

// Compatible is the controller in the M1 Pro's device tree.
const Compatible = "apple,t6000-aic"

const (
	INFO1   = 0x0004
	INFO3   = 0x000c
	CONFIG  = 0x0014
	IRQ_CFG = 0x2000

	CONFIG_ENABLE = 1 << 0

	EventIRQ = 1
	EventIPI = 4
)

// AIC is one controller.
type AIC struct {
	core, event                 mmio.Regs
	NrIRQ, MaxIRQ, NrDie        int
	swClr, maskSet, maskClr, hw uint64
	stride                      uint64
}

// New reads the controller's geometry (core: the "core" window; event:
// the "event" window).
func New(core, event mmio.Regs) (*AIC, error) {
	a := &AIC{core: core, event: event}
	i1, i3 := core.Read32(INFO1), core.Read32(INFO3)
	a.NrIRQ, a.NrDie, a.MaxIRQ = int(i1&0xffff), int(i1>>24&0xf)+1, int(i3&0xffff)
	if a.MaxIRQ == 0 || a.MaxIRQ%32 != 0 || a.NrIRQ > a.MaxIRQ {
		return nil, fmt.Errorf("aic: implausible geometry: INFO1 %#x INFO3 %#x", i1, i3)
	}
	words := uint64(a.MaxIRQ / 32 * 4)
	off := uint64(IRQ_CFG + 4*a.MaxIRQ) // IRQ_CFG
	a.swClr = off + words               // after SW_SET
	a.maskSet = a.swClr + words
	a.maskClr = a.maskSet + words
	a.hw = a.maskClr + words
	a.stride = a.hw + words - IRQ_CFG
	return a, nil
}

// Init masks every line, clears software-raised ones, and enables the
// controller.
func (a *AIC) Init() {
	for die := 0; die < a.NrDie; die++ {
		base := uint64(die) * a.stride
		for i := 0; i < a.MaxIRQ/32; i++ {
			a.core.Write32(base+a.maskSet+4*uint64(i), 0xffffffff)
			a.core.Write32(base+a.swClr+4*uint64(i), 0xffffffff)
		}
	}
	a.core.Write32(CONFIG, a.core.Read32(CONFIG)|CONFIG_ENABLE)
}

func (a *AIC) reg(off uint64, id int) (uint64, uint32) {
	die, n := uint64(id>>24&0xff), id&0xffff
	return uint64(die)*a.stride + off + uint64(4*(n>>5)), 1 << (n & 31)
}

// Enable unmasks a line (single-bit write to MASK_CLR: never
// read-modify-write a mask bank).
func (a *AIC) Enable(id int) {
	r, b := a.reg(a.maskClr, id)
	a.core.Write32(r, b)
}

// Disable masks a line (MASK_SET).
func (a *AIC) Disable(id int) {
	r, b := a.reg(a.maskSet, id)
	a.core.Write32(r, b)
}

// Pending reports a line's raw hardware state.
func (a *AIC) Pending(id int) bool {
	r, b := a.reg(a.hw, id)
	return a.core.Read32(r)&b != 0
}

// Event acknowledges the highest pending event: kind EventIRQ with an
// interrupt id (die<<24 | irq), EventIPI, or 0 when nothing is pending.
func (a *AIC) Event() (kind int, id int) {
	e := a.event.Read32(0)
	return int(e >> 16 & 0xff), int(e>>24&0xff)<<24 | int(e&0xffff)
}

// IRQs decodes a node's "interrupts" when its parent is an AIC:
// <type die irq flags> per interrupt (type 0 = IRQ; FIQs are skipped).
func IRQs(n *fdt.Node) ([]int, error) {
	v := n.U32s("interrupts")
	if len(v)%4 != 0 {
		return nil, errors.New("aic: interrupts not in 4-cell form")
	}
	var out []int
	for i := 0; i < len(v); i += 4 {
		if v[i] == 0 {
			out = append(out, int(v[i+1])<<24|int(v[i+2]))
		}
	}
	return out, nil
}
