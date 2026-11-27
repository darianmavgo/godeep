// ARM64 processor support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package arm64

import (
	"os"
	"os/signal"
	"syscall"
)

// IRQ_SIGNAL represents the `os/signal` used to signal and service
// interrupts.
const IRQ_SIGNAL = syscall.SIGTRAP

// defined in irq.s
func irq_enable()
func irq_disable()
func wfi()

// IRQHandler (godeep patch), when set, runs first in the IRQ exception, on
// the core that took it and on the exception's small private stack: it
// must be //go:nosplit and must not allocate, block or take locks. It
// acknowledges the interrupts itself (so the exception returns with IRQs
// still unmasked) and reports whether to relay IRQ_SIGNAL to the goroutine
// in ServiceInterrupts (or any signal.Notify of it).
//
// This lets the acknowledgement happen on the core the interrupt was routed
// to even when the servicing goroutine runs on another core (SMP), which a
// goroutine can't ensure: TamaGo can't pin one to a core.
var IRQHandler func() (relay bool)

// irqDispatch is called by handleInterrupt: 0 no handler (relay, then mask),
// 1 handled (relay), 2 handled (no relay).
//
//go:nosplit
func irqDispatch() uint64 {
	h := IRQHandler
	if h == nil {
		return 0
	}
	if h() {
		return 1
	}
	return 2
}

// EnableInterrupts unmasks IRQ interrupts.
func (cpu *CPU) EnableInterrupts() {
	irq_enable()
}

// DisableInterrupts masks IRQ interrupts.
func (cpu *CPU) DisableInterrupts() {
	irq_disable()
}

// WaitInterrupt suspends execution until an interrupt is received.
func (cpu *CPU) WaitInterrupt() {
	wfi()
}

// ServiceInterrupts puts the calling goroutine in wait state, its execution is
// resumed when an IRQ exception is received, an argument function can be set
// to service signaled interrupts (see gic package).
func (cpu *CPU) ServiceInterrupts(isr func()) {
	if isr == nil {
		isr = func() { return }
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, IRQ_SIGNAL)

	for {
		irq_enable()
		<-c
		isr()
	}
}
