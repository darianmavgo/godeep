//go:build tamago && arm64

package virt

import _ "unsafe"

// RAM starts at 0x40000000 on QEMU virt. The kernel image is linked at
// RamStart+0x10000 (see TEXT_START in the Makefile); TamaGo's arm64 MMU
// keeps its page tables at RamStart+0x4000..0x10000.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint64 = DRAM_BASE

// Must not exceed QEMU's -m (4G in the Makefile).
//
//go:linkname ramSize runtime/goos.RamSize
var ramSize uint64 = 0x100000000 // 4GB

//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint64 = 0x100
