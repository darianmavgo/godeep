//go:build tamago && arm64 && applem1

// Package applem1 is the godeep board for this MacBook Pro (14-inch 2021,
// Apple M1 Pro, t6000, J316sAP), booted by m1n1 (directly or via U-Boot).
// See docs/TargetMyMac.md. First milestone: output only. The shell's
// startup commands are typed by the board (autorun) and drawn on the
// framebuffer m1n1 leaves set up; there's no keyboard driver yet.
package applem1

import _ "unsafe"

// Memory map (16 GB model). DRAM starts at 1 TB. godeep uses a region well
// inside it: away from iBoot/m1n1/U-Boot at the bottom and the framebuffer
// and firmware carve-outs at the top. Boot checks it against the device
// tree's usable memory (devices.go). The kernel is linked at TEXT_START and
// relocates itself there from wherever the loader puts it (entry_arm64.s).
const (
	DRAM_BASE = 0x100_0000_0000
	DRAM_SIZE = 0x4_0000_0000 // 16 GB

	RAM_START = DRAM_BASE + 0x1_0000_0000 // +4 GB
	RAM_SIZE  = 0x1_0000_0000             // 4 GB for the Go runtime
	DMA_BASE  = RAM_START + RAM_SIZE      // DMA region right above it
	DMA_SIZE  = 0x2000_0000               // 512 MB

	// The boot device tree is copied to the start of the DMA region.
	FDT_RESERVE = 0x200000

	// The last MB of the DMA region keeps the console record across a
	// watchdog reset (kernel/pstore), if iBoot and m1n1 leave that RAM
	// alone: a fixed physical address every boot, checked by a magic and
	// size before it's trusted.
	PSTORE_SIZE = 0x100000
	PSTORE_BASE = DMA_BASE + DMA_SIZE - PSTORE_SIZE

	// Its first 8 KB hold the boot log (bootlog.go, kernel/pstore
	// BootLog*): the previous boot's half, then this boot's, written from
	// the first instruction of entry_arm64.s on (BOOTLOG_* there). The
	// console record (pstore.Ring) gets the rest.
	BOOTLOG_PREV = PSTORE_BASE
	BOOTLOG_CUR  = PSTORE_BASE + 0x1000
	BOOTLOG_SIZE = 0x2000
)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint64 = RAM_START

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint64 = RAM_SIZE

//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint64 = 0x100
