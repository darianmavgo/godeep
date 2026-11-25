//go:build tamago && arm64 && linkhwinit0 && !applem1

package virt

import (
	"runtime"
	_ "unsafe"

	"godeep/kernel/pgtable"
)

// godeep's MMU setup (replaces TamaGo's arm64 Hwinit0 via the linkhwinit0
// build tag, a godeep patch to the vendored TamaGo): 4-level, 48-bit
// identity-mapped tables from kernel/pgtable, the same code board/applem1
// uses to reach RAM above 1 TB. Runs before the Go runtime, on cpuinit's
// early identity map: no allocation (pt is static, physMem is zero-size).

type physMem struct{}

var pm physMem

func (*physMem) Read64(a uint64) uint64     { return read64(a) }
func (*physMem) Write64(a uint64, v uint64) { write64(a, v) }

var pt pgtable.Builder

// defined in mmu_arm64.s
func read64(addr uint64) uint64
func write64(addr uint64, v uint64)
func fp_enable()
func mmu_switch(ttbr0 uint64)

const tableArena = 0x4000 // tables from RamStart+16 KB up to the kernel text (as TamaGo)

//go:linkname hwinit0 runtime/goos.Hwinit0
func hwinit0() {
	fp_enable()

	ramStart, ramEnd := runtime.MemRegion()
	textStart, textEnd := runtime.TextRegion()
	textEnd = (textEnd + 0xfff) &^ 0xfff

	pt = pgtable.Builder{Mem: &pm, ArenaNext: ramStart + tableArena, ArenaEnd: textStart &^ 0xfff, Limit: 1 << 39}
	check(pt.Add(ramStart, textEnd, pgtable.NormalExec)) // vectors + kernel code
	check(pt.Add(textEnd, ramEnd, pgtable.Normal))       // Go heap, stack
	check(pt.Add(DMA_BASE, DMA_BASE+DMA_SIZE, pgtable.Normal))
	root, err := pt.Build()
	check(err)
	mmu_switch(root) // cpuinit already turned the MMU on with an early map

}

// check halts with a message on the UART (it works with the MMU off).
func check(err error) {
	if err == nil {
		return
	}
	for _, c := range []byte("godeep: MMU setup failed: " + err.Error() + "\r\n") {
		UART0.Tx(c)
	}
	for {
	}
}
