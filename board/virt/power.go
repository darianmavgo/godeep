//go:build tamago && arm64

package virt

import (
	"sync/atomic"
	"time"
	"unsafe"

	"godeep/kernel"
	"godeep/kernel/proc"
)

// QEMU virt wires its power button to PL061 GPIO line 3 (when the machine
// runs without ACPI: -machine virt,acpi=off). `system_powerdown` from the QEMU
// monitor/QMP raises the line for about 100ms.
const (
	PL061_BASE   = 0x09030000
	powerButton  = 3
	gpioDataAll  = 0x3fc // GPIODATA with all 8 address-mask bits set
	pollInterval = 20 * time.Millisecond
)

func readGPIO() uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Pointer(uintptr(PL061_BASE + gpioDataAll))))
}

// NewBoard returns the QEMU virt board: PL011 console, PL061 power button,
// and PSCI power-off.
func NewBoard() kernel.Board {
	shut := make(chan struct{})
	proc.Kthread("pwrbtn", func() {
		last := readGPIO()&(1<<powerButton) != 0
		for {
			time.Sleep(pollInterval)
			now := readGPIO()&(1<<powerButton) != 0
			if now && !last { // rising edge: button pressed
				close(shut)
				return
			}
			last = now
		}
	})
	return kernel.Board{
		Console:  NewConsole(),
		Shutdown: shut,
		PowerOff: func() { psciCall(psciSystemOff) },
	}
}
