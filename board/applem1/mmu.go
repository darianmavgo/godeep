//go:build tamago && arm64 && linkhwinit0 && applem1

package applem1

import (
	"runtime"
	_ "unsafe"

	"godeep/kernel/pgtable"
)

// MMU setup (Hwinit0), as board/virt but for RAM above 1 TB. DRAM outside
// godeep's region (firmware, m1n1, the framebuffer) is mapped Normal
// Non-cacheable, so the framebuffer is reachable wherever m1n1 put it
// without parsing the device tree before the Go runtime exists.

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

const tableArena = 0x4000

//go:linkname hwinit0 runtime/goos.Hwinit0
func hwinit0() {
	fp_enable()
	earlyScreen() // stage 1, and the fault screen from here on

	ramStart, ramEnd := runtime.MemRegion()
	textStart, textEnd := runtime.TextRegion()
	textEnd = (textEnd + 0xfff) &^ 0xfff

	pt = pgtable.Builder{Mem: &pm, ArenaNext: ramStart + tableArena, ArenaEnd: textStart &^ 0xfff, Limit: 1 << 41}
	check(pt.Add(DRAM_BASE, ramStart, pgtable.NormalNC)) // firmware, m1n1, U-Boot
	check(pt.Add(ramStart, textEnd, pgtable.NormalExec))
	check(pt.Add(textEnd, ramEnd, pgtable.Normal))
	check(pt.Add(DMA_BASE, DMA_BASE+DMA_SIZE, pgtable.Normal))
	check(pt.Add(DMA_BASE+DMA_SIZE, DRAM_BASE+DRAM_SIZE, pgtable.NormalNC)) // framebuffer, carve-outs
	root, err := pt.Build()
	check(err)
	mmu_switch(root)
	uartReady = uartBase != 0 // below Limit, so mapped as Device
	stage(StageTables)
}

// check: no console exists this early on the Mac. Stop here; the screen
// shows stage 1 only (or m1n1's output if there's no framebuffer).
func check(err error) {
	if err != nil {
		for {
		}
	}
}
