//go:build tamago && arm64 && !applem1

package virt

import (
	"unsafe"

	"godeep/kernel/fdt"
)

// The same Image runs on QEMU virt and on Parallels Desktop
// (docs/Parallels.md). Parallels has no device tree of its own: its UEFI
// loader (tools/efiboot) passes one written from its ACPI tables, with a
// different UART, GIC, PSCI conduit and no PL031. On Parallels an access to
// an address where no device answers stops the VM, so these come from the
// tree before anything touches a device; the defaults are QEMU virt's (an
// ELF boot has no tree).

var (
	rtcBase uint64      = PL031_BASE
	earlyFB fdt.EarlyFB // the tree's simple-framebuffer (Parallels: GOP)
	hasFB   bool
)

// scanEarly reads the device tree entry_arm64.s copied, without allocating.
func scanEarly() (fdt.Early, bool) {
	if bootFDT == 0 {
		return fdt.Early{}, false
	}
	hdr := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), 8)
	size := uint32(hdr[4])<<24 | uint32(hdr[5])<<16 | uint32(hdr[6])<<8 | uint32(hdr[7])
	if size > FDT_RESERVE {
		return fdt.Early{}, false
	}
	return fdt.ScanEarly(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), size)), true
}

// scanTree takes the UART, GIC, RTC, PSCI conduit and framebuffer from the
// tree. It runs first in Hwinit1: no allocation.
func scanTree() {
	e, ok := scanEarly()
	if !ok {
		return
	}
	if e.UART != 0 {
		UART0.Base = uintptr(e.UART)
	}
	if e.GICD != 0 {
		GICD_BASE, GICR_BASE = e.GICD, e.GICR
		GIC.GICD, GIC.GICR = uint32(e.GICD), uint32(e.GICR)
	}
	rtcBase = e.RTC
	if string(e.PSCI) == "smc" {
		psciMethod = "smc"
	}
	earlyFB, hasFB = e.FB, e.HasFB
}
