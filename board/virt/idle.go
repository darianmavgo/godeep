//go:build tamago && arm64 && !applem1

package virt

import (
	"math"
	"sync/atomic"

	"github.com/usbarmory/tamago/arm64/gic"
)

// GICv3: QEMU virt's (-machine virt,gic-version=3) unless the device tree
// says otherwise (Parallels Desktop; see early.go).
var (
	GICD_BASE uint64 = 0x08000000
	GICR_BASE uint64 = 0x080a0000
)

const (
	// Virtual timer PPI (INTID 27).
	VTIMER_IRQ = gic.FirstPPI + 11

	// Upper bound on one idle sleep. Devices are interrupt-driven, so this
	// is only a safety net, but with SMP it bounds a lost wake-up: an M
	// parked in semasleep (no deadline) whose goos.Wake goes astray. Some
	// still do under load (stop-the-world with 4 cores, cause not found);
	// 100 ms instead of 1 s made a 1M-line pipeline 0.4-0.8 s instead of
	// up to 1.6 s. Costs 10 wake-ups/s per idle core.
	maxIdle = 100_000_000 // 100 ms

)

// GIC's initializer must stay constant (static data): Hwinit1 runs before
// dynamic package initialization, and scanTree updates the addresses.
var GIC = &gic.GIC{GICD: 0x08000000, GICR: 0x080a0000}

// defined in idle_arm64.s
func read_cntfrq() uint64
func write_cntv_tval(ticks uint64)
func write_cntv_ctl(ctl uint64)
func wfi()
func read_cntvct() uint64

// initIdle routes the virtual timer through the GIC. Interrupts stay masked
// in PSTATE (no handler runs); a pending interrupt only wakes WFI.
func initIdle() {
	bootMPIDR = affinity(read_mpidr())
	CPU.DisableInterrupts()
	GIC.Init()
	GIC.EnableInterrupt(VTIMER_IRQ)
}

// idle is the runtime's idle hook (goos.Idle): sleep the core until the next
// timer deadline instead of spinning. TamaGo's default only sleeps when there
// is no deadline at all; with sleeping goroutines it busy-polls and burns a
// full host core.
//
// Under HVF, QEMU handles the WFI by sleeping the vCPU thread until
// CNTV_CVAL. Under TCG the timer interrupt reaches the core through the GIC.
func idle(until int64) {
	// The boot core keeps IRQs unmasked (irq.go) but sleeps with them
	// masked: an interrupt that comes between arming the timer and WFI
	// then stays pending and ends the WFI, instead of being taken first
	// and leaving WFI to sleep on. A wake-up taken before the mask is in
	// bootWoken. Secondaries are masked all the time.
	self := affinity(read_mpidr())
	boot := self == bootMPIDR
	atomic.AddUint32(&cntIdle[coreIndex(self)], 1)
	if boot {
		daif_mask_irq()
		defer daif_unmask_irq() // pending device interrupts are taken here
		if atomic.SwapUint32(&bootWoken, 0) != 0 {
			return
		}
	}
	d := int64(maxIdle)
	// until == 0 means "no timer pending" (not "already due"): sleep until
	// an interrupt or maxIdle. Treating it as due made an idle system with
	// no Go timers spin at 100% of a host core.
	if until > 0 && until != math.MaxInt64 {
		d = until - nanotime()
		if d <= 0 {
			return
		}
		d = min(d, maxIdle)
	}
	ticks := uint64(d) * read_cntfrq() / 1e9
	if ticks == 0 {
		return
	}
	write_cntv_tval(ticks)
	write_cntv_ctl(1) // ENABLE, not IMASK
	t0 := read_cntvct()
	wfi()
	write_cntv_ctl(0) // deassert the timer interrupt
	// Idle time (dev.CPULoad), counted only once the timer is deasserted:
	// with any work between WFI and that, check-virt's USB hub stopped
	// enumerating and Go timers stalled under QEMU HVF.
	atomic.AddUint64(&idleTicks[coreIndex(self)], read_cntvct()-t0)
	if !boot {
		drainLocal() // a secondary: acknowledge its SGI and timer here
	}
}
