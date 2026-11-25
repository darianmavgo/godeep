//go:build tamago && arm64

package virt

import (
	"math"

	"github.com/usbarmory/tamago/arm64/gic"
)

// GICv3 on QEMU virt (-machine virt,gic-version=3).
const (
	GICD_BASE = 0x08000000
	GICR_BASE = 0x080a0000

	// Virtual timer PPI (INTID 27).
	VTIMER_IRQ = gic.FirstPPI + 11

	// Upper bound on one idle sleep. Devices are interrupt-driven, so this
	// is only a safety net.
	maxIdle = 1_000_000_000 // 1s
)

var GIC = &gic.GIC{GICD: GICD_BASE, GICR: GICR_BASE}

// defined in idle_arm64.s
func read_cntfrq() uint64
func write_cntv_tval(ticks uint64)
func write_cntv_ctl(ctl uint64)
func wfi()

// initIdle routes the virtual timer through the GIC. Interrupts stay masked
// in PSTATE (no handler runs); a pending interrupt only wakes WFI.
func initIdle() {
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
	wfi()
	write_cntv_ctl(0) // deassert the timer interrupt
}
