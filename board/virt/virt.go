//go:build tamago && arm64

// Package virt is the godeep board for QEMU's arm64 `virt` machine. On Apple
// Silicon it runs under Hypervisor.framework (-accel hvf) at native speed.
//
// TamaGo ships generic arm64 support (boot, MMU, exceptions, generic timer)
// but no board for QEMU virt, so this package supplies the board hooks the
// TamaGo runtime links against: RAM layout, Hwinit1, Nanotime, Printk and the
// RNG.
package virt

import (
	"runtime/goos"
	_ "unsafe"

	"github.com/usbarmory/tamago/arm64"
)

// QEMU virt memory map (hw/arm/virt.c).
const (
	PL011_BASE = 0x09000000 // UART0
	DRAM_BASE  = 0x40000000
)

// CPU is the boot core. (TamaGo has no arm64 SMP bring-up yet: one core.)
var CPU = &arm64.CPU{
	// required before Init()
	TimerOffset: 1,
}

// UART0 is the serial console.
var UART0 = &PL011{Base: PL011_BASE}

// Init runs early in runtime setup (after the MMU is on, before main).
//
//go:linkname Init runtime/goos.Hwinit1
func Init() {
	CPU.Init()
	CPU.EnableCache()
	// The hypervisor programs CNTFRQ; just derive the ns multiplier from it.
	CPU.InitGenericTimers(0, 0)
	UART0.Init()

	// Sleep instead of spinning when the scheduler is idle.
	initIdle()
	goos.Idle = idle

	// Power off through PSCI instead of parking the core, so QEMU exits.
	goos.Exit = func(int32) {
		psciCall(psciSystemOff)
		for {
		}
	}
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return CPU.GetTime()
}
