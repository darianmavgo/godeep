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
	"sync/atomic"
	"unsafe"

	"github.com/usbarmory/tamago/arm64"
)

// QEMU virt memory map (hw/arm/virt.c).
const (
	PL011_BASE        = 0x09000000 // UART0
	PL031_BASE        = 0x09010000 // RTC
	PL061_BASE        = 0x09030000 // GPIO (power button on line 3)
	VIRTIO_MMIO_BASE  = 0x0a000000
	VIRTIO_MMIO_SLOTS = 32
	DRAM_BASE         = 0x40000000

	// GIC interrupt IDs: SPIs start at 32 (hw/arm/virt.c irqmap).
	UART0_IRQ  = 32 + 1
	RTC_IRQ    = 32 + 2
	GPIO_IRQ   = 32 + 7
	VIRTIO_IRQ = 32 + 16 // + slot
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

	// The DMA region is outside the Go heap, so the MMU maps it as device
	// memory by default. Make it normal (cacheable) memory: virt devices are
	// cache-coherent.
	CPU.ConfigureMMU(DMA_BASE, DMA_BASE+DMA_SIZE, 0,
		arm64.MemoryAttributes|arm64.TTE_BLOCK|arm64.TTE_EXECUTE_NEVER)

	// Wall clock from the PL031 RTC (seconds since 1970), before any Go
	// timer exists: SetTime shifts the monotonic clock too.
	CPU.SetTime(int64(atomic.LoadUint32((*uint32)(unsafe.Pointer(uintptr(PL031_BASE))))) * 1e9)

	// Sleep instead of spinning when the scheduler is idle.
	initIdle()
	goos.Idle = idle

	// Power off through PSCI instead of parking the core, so QEMU exits.
	goos.Exit = func(int32) {
		psciPowerOff()
		for {
		}
	}
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return CPU.GetTime()
}
