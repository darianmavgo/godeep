//go:build tamago && arm64 && applem1

package applem1

import (
	"context"
	"fmt"
	"math/bits"
	"runtime"
	"runtime/goos"
	"sync/atomic"
	"time"

	"godeep/kernel/dev"
)

// Idle without interrupts: WFE woken by the generic timer's event stream.
//
// WFI needs an interrupt to wake up, and on Apple cores the timer
// interrupt is a FIQ whose delivery to EL1 under a VHE EL2 is gated by an
// Apple register (VM_TMR_FIQ_ENA_EL2); with no AIC2 driver yet that path
// is unproven. The event stream needs none of it: CNTKCTL_EL1.EVNTEN makes
// the counter send a WFE wake-up event every time one of its bits flips, an
// architectural feature (Linux turns it on for every arm64 CPU, M1 included).
// The core sleeps in WFE and wakes every ~0.7 ms to check the deadline:
// about 1,500 wake-ups a second instead of spinning.
//
// When AIC2 lands, this becomes board/virt's WFI + timer-deadline version.
// Under QEMU TCG (tools/fakem1n1) WFE doesn't sleep, so it still spins there.

const (
	// Upper bound on one idle call, so the runtime re-checks regularly.
	maxIdle = int64(time.Second)

	cntkctlEVNTEN  = 1 << 2
	cntkctlEVNTDIR = 1 << 3
	cntkctlEVNTI   = 0xf << 4

	idleCompatible = "godeep,wfe-idle"
)

// defined in idle_arm64.s
func read_cntfrq() uint64
func read_cntvct() uint64
func read_cntkctl() uint64
func write_cntkctl(v uint64)
func wfe()

var (
	evntPeriod uint64 // event stream period, counter ticks
	idleBase   uint64 // counter when idle accounting started
	idleTicks  atomic.Uint64
	idleWakes  atomic.Uint64
)

// initIdle turns on the event stream, about one event per millisecond or
// faster, and installs the idle hook.
func initIdle() {
	// An event every 2^(EVNTI+1) ticks; pick the largest period <= 1 ms.
	n := bits.Len64(read_cntfrq()/1000) - 2
	n = max(0, min(n, 15))
	write_cntkctl(read_cntkctl()&^(cntkctlEVNTI|cntkctlEVNTDIR) | uint64(n)<<4 | cntkctlEVNTEN)
	evntPeriod = 1 << (n + 1)
	idleBase = read_cntvct()
	goos.Idle = idle
}

// idle is the runtime's idle hook (goos.Idle). until == 0 means "no timer
// pending": sleep maxIdle. With one core nothing but a timer can make a
// goroutine runnable while it idles (no interrupts yet); with several,
// goos.Wake (smp.go) also ends the sleep.
func idle(until int64) {
	end := nanotime() + maxIdle
	if until > 0 && until < end {
		end = until
	}
	c0 := read_cntvct()
	var n uint64
	woken := &smpWoken[coreIndex()]
	for nanotime() < end {
		if atomic.SwapUint32(woken, 0) != 0 {
			break // goos.Wake: there's work for this core
		}
		wfe()
		n++
	}
	idleTicks.Add(read_cntvct() - c0)
	idleWakes.Add(n)
}

// idleStats is lsdev's view of the idle loop. If WFE didn't sleep, the
// wake-up rate would be in the millions per second, not ~1,500.
type idleStats struct{}

func (idleStats) Devices() []string {
	total := read_cntvct() - idleBase
	if total == 0 || evntPeriod == 0 {
		return []string{"not started"}
	}
	freq := read_cntfrq()
	secs := float64(total) / float64(freq)
	return []string{SMPInfo(), fmt.Sprintf("WFE + event stream every %v: %.1f%% idle (all cores), %.0f wake-ups/s",
		time.Duration(evntPeriod*uint64(time.Second)/freq).Round(time.Microsecond),
		100*float64(idleTicks.Load())/float64(total)/float64(max(1, runtime.GOMAXPROCS(0))), float64(idleWakes.Load())/secs)}
}

func runIdleStats(ctx context.Context, d *dev.Device) error {
	d.Ready(dev.Lister(idleStats{}))
	<-ctx.Done()
	return nil
}
