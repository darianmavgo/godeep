//go:build tamago && arm64 && !applem1

package virt

import (
	"sync/atomic"
	"unsafe"
)

// Interrupt flow with several cores. Device interrupts (SPIs) are routed
// to the boot core, whose IRQs stay unmasked; the exception runs irqTop
// there, which acknowledges each interrupt on this core's GIC CPU
// interface (the only place it can be acknowledged), masks the line at the
// distributor, signals end of interrupt and records the line in pendingIRQ.
// The "irq" goroutine (serviceInterrupts) then dispatches recorded lines to
// their drivers from whatever core it runs on: everything after the
// acknowledgement goes through the distributor, which every core reaches.
// Secondary cores keep IRQs masked: their wake-up SGIs and timer only end
// WFI, and their idle loop acknowledges them (drainLocal).

// pendingIRQ is the set of lines irqTop masked and recorded (1024 lines).
var pendingIRQ [32]uint32

// bootMPIDR is the boot core's affinity, for GICD_IROUTER (Aff3 at 39:32,
// Aff2..0 at 23:0; never IRM, bit 31, which MPIDR's RES1 bit 31 would set).
var bootMPIDR uint64

// SGI used to wake a core from WFI (goos.Wake).
const wakeSGI = 0

// bootWoken is set when the boot core's exception acknowledged a wake-up
// SGI. That core's IRQs are unmasked, so an SGI arriving after semasleep
// checked its counter but before idle's WFI is taken (and acknowledged) at
// once; idle checks this flag after masking IRQs, or the wake-up is lost
// and the core sleeps until its next timer.
var bootWoken uint32

// defined in irq_arm64.s
func icc_iar0() uint64
func icc_eoir0(id uint64)
func read_mpidr() uint64
func icc_sgi0r(v uint64)
func daif_mask_irq()
func daif_unmask_irq()

func affinity(mpidr uint64) uint64 { return mpidr&0xffffff | (mpidr>>32&0xff)<<32 }

func w64(addr uint64, v uint64) {
	atomic.StoreUint64((*uint64)(unsafe.Pointer(uintptr(addr))), v)
}

// irqTop runs inside the IRQ exception (arm64.IRQHandler), on the core that
// took it, on the exception's own small stack: no allocation, no locks, no
// calls that could grow the stack.
//
//go:nosplit
func irqTop() bool {
	relay := false
	for {
		id := icc_iar0() & 0xffffff
		if id >= 1020 { // spurious: nothing more pending
			return relay
		}
		switch {
		case id == VTIMER_IRQ:
			write_cntv_ctl(0) // the idle governor's wake-up: deassert it
			atomic.AddUint32(&cntTimer[coreIndex(bootMPIDR)], 1)
		case id >= 32:
			bank, bit := id/32, uint32(1)<<(id%32)
			atomic.StoreUint32((*uint32)(unsafe.Pointer(uintptr(GICD_BASE+gicdICENABLER+4*bank))), bit)
			atomic.OrUint32(&pendingIRQ[bank], bit)
			relay = true
		}
		if id < 16 { // a wake-up SGI: see bootWoken
			atomic.StoreUint32(&bootWoken, 1)
			atomic.AddUint32(&cntSGI[coreIndex(bootMPIDR)], 1)
		}
		icc_eoir0(id)
	}
}

// drainLocal acknowledges what woke a core whose IRQs stay masked (a
// secondary): wake-up SGIs and its timer. Device interrupts never route here.
//
//go:nosplit
func drainLocal() {
	for {
		id := icc_iar0() & 0xffffff
		if id >= 1020 {
			return
		}
		self := coreIndex(affinity(read_mpidr()))
		if id == VTIMER_IRQ {
			write_cntv_ctl(0)
			atomic.AddUint32(&cntTimer[self], 1)
		} else if id < 16 {
			atomic.AddUint32(&cntSGI[self], 1)
		}
		icc_eoir0(id)
	}
}
