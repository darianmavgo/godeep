//go:build tamago && arm64 && applem1

package applem1

import (
	"sync/atomic"
	"unsafe"

	"github.com/usbarmory/tamago/arm64"

	"godeep/drivers/simplefb"
	"godeep/kernel/fdt"
)

// Boot stages, drawn as numbered squares along the top of m1n1's framebuffer
// until the console takes over the screen. On a frozen screen, the last
// square is the last stage reached:
//
//	(none)  godeep never reached Go code: entry, relocation, EL drop or the
//	        early identity map (entry_arm64.s), or no framebuffer in the tree
//	1 blue   Hwinit0: Go code runs, device tree read, framebuffer mapped
//	2 green  final page tables live (kernel/pgtable)
//	3 yellow Hwinit1: CPU, caches and timers initialized
//	4 purple Go runtime up (heap, goroutines): the board is being set up
//
// An exception from stage 1 until the console exists paints the screen red
// with ESR/ELR/FAR and the stage (simplefb.Early.Fault).
const (
	StageGo = iota + 1
	StageTables
	StageHwinit1
	StageRuntime
)

// Early vector table and fault stack: free space below the page-table arena
// (mmu.go tableArena), above TamaGo's vectors (RamStart) and the boot tables
// (RamStart+0x1000/0x2000) that cpuinit builds.
const (
	earlyVectors = 0x800  // RamStart offset (entry_arm64.s)
	faultStack   = 0x4000 // RamStart offset, top (early_arm64.s)
	bootL1       = 0x2000 // RamStart offset (entry_arm64.s)
)

var (
	screen    simplefb.Early
	bootStage int
	uartBase  uint64 // PL011 from the tree (QEMU's m1n1 stand-in), 0 on the Mac
	uartReady bool   // mapped (final tables live)

	// written by earlyFault (early_arm64.s)
	faultESR, faultELR, faultFAR, faultDepth uint64
)

// defined in early_arm64.s
func earlyFault()
func read_esr() uint64
func read_far() uint64
func tables_sync()

// testFaultAt is set by "godeep.testfault=N" in /chosen/bootargs: reaching
// stage N then reads an unmapped address, to check the fault screen
// (tools/fakem1n1 check -fault N). 0: off.
var testFaultAt int

// testFaultAddr is above both the boot tables and the final tables' Limit.
const testFaultAddr = 0x3ff_dead_0000

// stage records and draws boot stage n.
//
//go:nosplit
func stage(n int) {
	bootStage = n
	screen.Mark(n)
	if n == testFaultAt {
		read64(testFaultAddr)
	}
}

// parseTestFault finds godeep.testfault=N (one digit) without allocating.
func parseTestFault(args []byte) int {
	const key = "godeep.testfault="
	for i := 0; i+len(key) < len(args); i++ {
		if (i == 0 || args[i-1] == ' ') && string(args[i:i+len(key)]) == key {
			if d := args[i+len(key)]; d >= '1' && d <= '9' {
				return int(d - '0')
			}
		}
	}
	return 0
}

// earlyScreen runs first in Hwinit0, on cpuinit's identity map (which covers
// godeep's RAM and DMA regions, so the device-tree copy is readable). It finds
// the framebuffer and maps it into the boot tables so stage marks and the
// fault screen work before the final tables exist.
func earlyScreen() {
	if bootFDT == 0 {
		return
	}
	hdr := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), 8)
	size := uint32(hdr[4])<<24 | uint32(hdr[5])<<16 | uint32(hdr[6])<<8 | uint32(hdr[7])
	if size > FDT_RESERVE {
		return
	}
	e := fdt.ScanEarly(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), size))
	uartBase = e.UART
	testFaultAt = parseTestFault(e.Bootargs)
	if !e.HasFB || !mapEarly(e.FB.Base, e.FB.Size) {
		return
	}
	pix := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(e.FB.Base))), e.FB.Size)
	s, ok := simplefb.NewEarly(pix, int(e.FB.Width), int(e.FB.Height), int(e.FB.Stride), e.FB.Format)
	if !ok {
		return
	}
	screen = s
	stage(StageGo)
}

// mapEarly adds [base, base+size) to cpuinit's boot tables as 1 GB Normal
// non-cacheable blocks (as the final tables map it, mmu.go). Only GBs under
// the same L0 entry as RAM, outside godeep's own region and not yet mapped.
func mapEarly(base, size uint64) bool {
	const (
		gb      = 1 << 30
		ncBlock = 1<<54 | 1<<53 | 1<<10 | 2<<8 | 2<<2 | 1 // UXN PXN AF outer-shareable MAIR[2] block
	)
	if size == 0 || base < DRAM_BASE || base+size > DRAM_BASE+DRAM_SIZE ||
		base>>39 != RAM_START>>39 || (base+size-1)>>39 != RAM_START>>39 {
		return false
	}
	l1 := uint64(RAM_START + bootL1)
	for g := base &^ (gb - 1); g < base+size; g += gb {
		if g < DMA_BASE+DMA_SIZE && g+gb > RAM_START {
			return false // overlaps godeep's own region: the tree is wrong
		}
		slot := l1 + (g>>30&511)*8
		if read64(slot) == 0 {
			write64(slot, g|ncBlock)
		}
	}
	tables_sync()
	return true
}

// earlyFaultGo paints the fault screen (called by earlyFault on its own stack).
//
//go:nosplit
func earlyFaultGo() {
	screen.Fault(bootStage, faultESR, faultELR, faultFAR)
}

// lateFault replaces TamaGo's exception handler (from Hwinit1): until the
// console exists, a fault would otherwise print to nowhere.
func lateFault(pc uintptr) {
	stopWatchdog()
	if console == nil && atomic.AddUint64(&faultDepth, 1) == 1 {
		screen.Fault(bootStage, read_esr(), uint64(pc), read_far())
	}
	arm64.DefaultExceptionHandler(pc)
}

// uartPut writes to the PL011 that QEMU's m1n1 stand-in describes, so the
// console is mirrored to the terminal there. The Mac's tree has none.
func uartPut(c byte) {
	if !uartReady {
		return
	}
	fr := (*uint32)(unsafe.Pointer(uintptr(uartBase + 0x18)))
	dr := (*uint32)(unsafe.Pointer(uintptr(uartBase)))
	if c == '\n' {
		for atomic.LoadUint32(fr)&(1<<5) != 0 { // TXFF
		}
		atomic.StoreUint32(dr, '\r')
	}
	for atomic.LoadUint32(fr)&(1<<5) != 0 {
	}
	atomic.StoreUint32(dr, uint32(c))
}
