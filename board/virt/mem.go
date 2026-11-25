//go:build tamago && arm64 && !applem1

package virt

import _ "unsafe"

// RAM starts at 0x40000000 on QEMU virt. The kernel image is linked at
// RamStart+0x10000 (see TEXT_START in the Makefile); TamaGo's arm64 MMU
// keeps its page tables at RamStart+0x4000..0x10000.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint64 = DRAM_BASE

// The Go heap gets the first 3.5 GB; the last 512 MB of QEMU's -m 4G is the
// DMA region (outside the GC's reach), see DMA_BASE.
//
//go:linkname ramSize runtime/goos.RamSize
var ramSize uint64 = 0xe0000000 // 3.5GB

// DMA region: RAM the Go runtime doesn't know about.
const (
	DMA_BASE = DRAM_BASE + 0xe0000000
	DMA_SIZE = 0x20000000 // 512MB

	// The boot device tree is copied to the start of the DMA region by
	// cpuinit (entry_arm64.s, FDT_COPY); the allocator gets the rest.
	FDT_RESERVE = 0x200000 // 2MB
)

//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint64 = 0x100
