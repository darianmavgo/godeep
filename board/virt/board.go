//go:build tamago && arm64 && !applem1

package virt

import (
	"unsafe"

	"github.com/usbarmory/tamago/arm64/gic"

	"godeep/kernel"
	"godeep/kernel/dma"
	"godeep/kernel/irq"
	"godeep/kernel/proc"
)

// gicController adapts TamaGo's GICv3 driver to irq.Controller.
//
// Disable does NOT use GIC.DisableInterrupt: TamaGo writes GICD_ICENABLER
// with a read-modify-write, and since that register is write-1-to-clear and
// reads back every enabled interrupt in the bank, disabling one line disables
// all its neighbours (masking the UART silently disabled the power button).
// Writing only the line's bit is the correct use of the register.
// (Enable via TamaGo is fine: writing back 1s to ISENABLER is harmless, and
// it also sets the line's group and routing.)
type gicController struct{}

const (
	gicdICENABLER = 0x180
	gicrSGIBase   = 0x10000
)

func (gicController) Enable(id int) { GIC.EnableInterrupt(id) }

func (gicController) Disable(id int) {
	bank, bit := uint64(id/32), uint32(1)<<(id%32)
	if id < gic.FirstSPI { // SGIs/PPIs live in this CPU's redistributor
		w32(GICR_BASE+gicrSGIBase+gicdICENABLER+4*bank, bit)
	} else {
		w32(GICD_BASE+gicdICENABLER+4*bank, bit)
	}
}

// Bridge is the interrupt bridge for this board.
var Bridge = irq.NewBridge(gicController{})

// serviceInterrupts is the "irq" kernel thread: TamaGo's exception handler
// wakes it with interrupts masked; it acknowledges every pending interrupt
// and hands each to the bridge, which masks the line and signals its driver.
func serviceInterrupts() {
	CPU.ServiceInterrupts(func() {
		for {
			id := GIC.GetInterrupt()
			if id >= gic.FirstSIN {
				return // spurious: nothing more pending
			}
			if id == VTIMER_IRQ {
				continue // the idle governor's wakeup timer; idle disarms it
			}
			Bridge.Dispatch(id)
		}
	})
}

// shutdown is closed by the power-button driver.
var shutdown = make(chan struct{})

// NewBoard returns the QEMU virt board.
func NewBoard() kernel.Board {
	proc.Kthread("irq", serviceInterrupts)
	nodes, source := devices()
	return kernel.Board{
		Info:     BootInfo(source),
		Console:  NewConsole(Bridge),
		Shutdown: shutdown,
		PowerOff: psciPowerOff,
		IRQ:      Bridge,
		// The first FDT_RESERVE bytes hold the boot device tree (entry_arm64.s).
		DMA: dma.New(dma.Region{
			Base: DMA_BASE + FDT_RESERVE,
			Mem:  unsafe.Slice((*byte)(unsafe.Pointer(uintptr(DMA_BASE+FDT_RESERVE))), DMA_SIZE-FDT_RESERVE),
		}),
		Devices: nodes,
	}
}
