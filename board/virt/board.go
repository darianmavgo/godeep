//go:build tamago && arm64 && !applem1

package virt

import (
	"fmt"
	"io"
	"unsafe"

	"github.com/usbarmory/tamago/arm64/gic"

	"godeep/drivers/qemu/fwcfg"
	"godeep/drivers/simplefb"
	"godeep/kernel"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/irq"
	kmmio "godeep/kernel/mmio"
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
	// The first FDT_RESERVE bytes hold the boot device tree (entry_arm64.s).
	alloc := dma.New(dma.Region{
		Base: DMA_BASE + FDT_RESERVE,
		Mem:  unsafe.Slice((*byte)(unsafe.Pointer(uintptr(DMA_BASE+FDT_RESERVE))), DMA_SIZE-FDT_RESERVE),
	})
	info := BootInfo(source)
	con := NewConsole(Bridge)
	if fb, note := framebuffer(nodes, alloc); fb != nil {
		// Text goes to both the framebuffer and the UART; the USB mouse
		// drives the framebuffer console (pointer, scrollback, select/paste).
		con.Out = tee{fb, con.Out}
		con.Input = &simplefb.Mouse{C: fb, Paste: simplefb.PasteTo(consoleIn)}
		nodes = append(nodes, dev.Node{Name: "fbcon", Compatible: simplefb.DeviceCompatible, Config: fb})
		info += "; " + note
	} else if note != "" {
		info += "; " + note
	}
	return kernel.Board{
		Info:     info,
		Console:  con,
		Shutdown: shutdown,
		PowerOff: psciPowerOff,
		IRQ:      Bridge,
		DMA:      alloc,
		Devices:  nodes,
		LogLabel: "GODEEPDATA", // like the Mac: a disk with that FAT32 label gets the log
	}
}

// Framebuffer size for QEMU's ramfb.
const fbWidth, fbHeight = 1280, 800

// framebuffer sets up a console on QEMU's ramfb (-device ramfb), through
// the fw_cfg device in the tree. No ramfb: nil and no note.
func framebuffer(nodes []dev.Node, a *dma.Allocator) (*simplefb.Console, string) {
	for _, n := range nodes {
		if n.Compatible != fwcfg.Compatible || len(n.Reg) == 0 {
			continue
		}
		f, err := fwcfg.New(kmmio.Window{Base: n.Reg[0].Base}, a)
		if err != nil {
			return nil, "fw_cfg: " + err.Error()
		}
		defer f.Close()
		if _, err := f.Find("etc/ramfb"); err != nil {
			return nil, "" // QEMU started without -device ramfb
		}
		r, err := f.SetupRAMFB(a, fbWidth, fbHeight)
		if err != nil {
			return nil, "ramfb: " + err.Error()
		}
		c, err := simplefb.NewConsole(&simplefb.Framebuffer{Pix: r.Pix, Width: r.Width, Height: r.Height,
			Stride: r.Stride, Format: simplefb.X8R8G8B8}, 0)
		if err != nil {
			return nil, "ramfb: " + err.Error()
		}
		return c, fmt.Sprintf("ramfb %dx%d console", r.Width, r.Height)
	}
	return nil, ""
}

// tee writes the console to the framebuffer and the UART.
type tee struct{ fb, uart io.Writer }

func (t tee) Write(p []byte) (int, error) {
	t.fb.Write(p)
	return t.uart.Write(p)
}
