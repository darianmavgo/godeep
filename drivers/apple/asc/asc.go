// Package asc drives an Apple ASC: a coprocessor (ANS2 for the SSD, the
// SMC, ...) with a CPU control register and a mailbox that carries RTKit
// messages, after m1n1's asc.c. Polled: a goroutine drains the
// coprocessor-to-AP queue every PollFast while messages flow and every
// PollIdle otherwise, until AIC2 interrupts exist.
package asc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"godeep/kernel/irq"
	"godeep/kernel/mmio"
	"godeep/kernel/rtkit"
)

// CPU control (the ASC's first register window).
const (
	CPU_CONTROL = 0x44
	CPU_RUN     = 1 << 4
)

// Mailbox registers (the ASC window + 0x8000, or the mailbox node's own).
const (
	A2I_CONTROL = 0x110
	A2I_SEND0   = 0x800
	A2I_SEND1   = 0x808
	I2A_CONTROL = 0x114
	I2A_RECV0   = 0x830
	I2A_RECV1   = 0x838

	CONTROL_FULL  = 1 << 16
	CONTROL_EMPTY = 1 << 17

	PollFast    = time.Millisecond
	PollIdle    = 20 * time.Millisecond
	SendTimeout = 200 * time.Millisecond
)

// CPURunning, StartCPU and StopCPU work on the CPU control window.
func CPURunning(cpu mmio.Regs) bool { return cpu.Read32(CPU_CONTROL)&CPU_RUN != 0 }
func StartCPU(cpu mmio.Regs)        { cpu.Write32(CPU_CONTROL, cpu.Read32(CPU_CONTROL)|CPU_RUN) }
func StopCPU(cpu mmio.Regs)         { cpu.Write32(CPU_CONTROL, cpu.Read32(CPU_CONTROL)&^CPU_RUN) }

// Mailbox is an rtkit.Mailbox over the ASC's registers.
type Mailbox struct {
	r    mmio.Regs64
	mu   sync.Mutex // one sender at a time
	recv chan rtkit.Message

	Sent, Received atomic.Uint64
}

var _ rtkit.Mailbox = (*Mailbox)(nil)

// New returns the mailbox; Run must be running for Recv to deliver.
func New(r mmio.Regs64) *Mailbox {
	return &Mailbox{r: r, recv: make(chan rtkit.Message, 64)}
}

// Send waits for room in the AP-to-coprocessor queue and posts m: the
// payload, then the endpoint word, which sends it.
func (m *Mailbox) Send(ctx context.Context, msg rtkit.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	deadline := time.Now().Add(SendTimeout)
	for m.r.Read32(A2I_CONTROL)&CONTROL_FULL != 0 {
		if time.Now().After(deadline) {
			return errors.New("asc: mailbox full")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		time.Sleep(10 * time.Microsecond)
	}
	m.r.Write64(A2I_SEND0, msg.Data)
	m.r.Write64(A2I_SEND1, uint64(msg.EP))
	m.Sent.Add(1)
	return nil
}

func (m *Mailbox) Recv() <-chan rtkit.Message { return m.recv }

// Run drains the coprocessor-to-AP queue until ctx is done: when line
// (the mailbox's recv-not-empty interrupt) fires, or by polling if line is
// nil. With a line it still checks every PollIdle, in case an edge is lost.
func (m *Mailbox) Run(ctx context.Context, line *irq.Line) {
	if line != nil {
		defer line.Release()
		t := time.NewTicker(PollIdle)
		defer t.Stop()
		line.Done() // unmask
		for {
			select {
			case <-ctx.Done():
				return
			case <-line.C:
			case <-t.C:
			}
			if !m.drain(ctx) {
				return
			}
			line.Done()
		}
	}
	idle := 0
	for {
		before := m.Received.Load()
		if !m.drain(ctx) {
			return
		}
		got := m.Received.Load() != before
		wait := PollFast
		if got {
			idle = 0
		} else if idle++; idle > 100 {
			wait = PollIdle
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// RecvLine requests the mailbox's recv-not-empty interrupt (the fourth of
// its "interrupts": send-empty, send-not-empty, recv-empty,
// recv-not-empty) from the board's bridge; nil means poll.
func RecvLine(b *irq.Bridge, irqs []int, name string) *irq.Line {
	if b == nil || len(irqs) < 4 {
		return nil
	}
	l, err := b.Request(irqs[3], name)
	if err != nil {
		return nil
	}
	return l
}

// drain hands every queued message to Recv; false if ctx ended.
func (m *Mailbox) drain(ctx context.Context) bool {
	for m.r.Read32(I2A_CONTROL)&CONTROL_EMPTY == 0 {
		d := m.r.Read64(I2A_RECV0)
		ep := uint8(m.r.Read64(I2A_RECV1))
		select {
		case m.recv <- rtkit.Message{EP: ep, Data: d}:
			m.Received.Add(1)
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// SendNow posts a message without waiting on other goroutines (for the
// last act before power goes: no locks, no timers). It gives up after
// about a second of a full queue.
func (m *Mailbox) SendNow(msg rtkit.Message) bool {
	for i := 0; m.r.Read32(A2I_CONTROL)&CONTROL_FULL != 0; i++ {
		if i > 1e7 {
			return false
		}
	}
	m.r.Write64(A2I_SEND0, msg.Data)
	m.r.Write64(A2I_SEND1, uint64(msg.EP))
	return true
}
