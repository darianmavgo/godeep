//go:build tamago && arm64 && !applem1

package virt

import (
	"fmt"
	"io"
	mbits "math/bits"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/usbarmory/tamago/arm64"
	"github.com/usbarmory/tamago/arm64/gic"

	"godeep/drivers/qemu/fwcfg"
	"godeep/drivers/simplefb"
	"godeep/kernel"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/irq"
	kmmio "godeep/kernel/mmio"
	"godeep/kernel/proc"
	"godeep/kernel/pstore"
)

// gicController adapts TamaGo's GICv3 driver to irq.Controller.
//
// Disable does NOT use GIC.DisableInterrupt: TamaGo writes GICD_ICENABLER
// with a read-modify-write, and since that register is write-1-to-clear and
// reads back every enabled interrupt in the bank, disabling one line disables
// all its neighbours (masking the UART silently disabled the power button).
// Writing only the line's bit is the correct use of the register.
// Enable doesn't use TamaGo either: its read-modify-write of ISENABLER can
// write back the enable bit of a line the IRQ exception (irqTop) masked in
// between, on this core or another.
type gicController struct{}

const (
	gicdIGROUPR   = 0x080
	gicdISENABLER = 0x100
	gicdICENABLER = 0x180
	gicdIROUTER   = 0x6000
	gicrSGIBase   = 0x10000
)

func (gicController) Enable(id int) {
	bank, bit := uint64(id/32), uint32(1)<<(id%32)
	if id < gic.FirstSPI {
		w32(GICR_BASE+gicrSGIBase+gicdISENABLER+4*bank, bit)
		return
	}
	// route to the boot core, Group 0 (as TamaGo's GIC.Init configures the
	// CPU interface); then set only this line's enable bit
	w64(GICD_BASE+gicdIROUTER+8*uint64(id), bootMPIDR)
	gicMu.Lock()
	w32(GICD_BASE+gicdIGROUPR+4*bank, r32(GICD_BASE+gicdIGROUPR+4*bank)&^bit)
	gicMu.Unlock()
	w32(GICD_BASE+gicdISENABLER+4*bank, bit)
}

// gicMu serializes read-modify-writes of the group registers (nothing in an
// exception touches them).
var gicMu sync.Mutex

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

// serviceInterrupts is the "irq" kernel thread. The IRQ exception itself
// (irqTop, on the core the GIC routed the interrupt to) acknowledges it,
// masks the line and records it; this goroutine, on whichever core it runs,
// hands each recorded line to the bridge, which signals its driver. The
// driver's line.Done() unmasks the line again.
func serviceInterrupts() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, arm64.IRQ_SIGNAL)
	for {
		for i := range pendingIRQ {
			bits := atomic.SwapUint32(&pendingIRQ[i], 0)
			for bits != 0 {
				b := mbits.TrailingZeros32(bits)
				bits &^= 1 << b
				Bridge.Dispatch(32*i + b)
			}
		}
		<-c
	}
}

// shutdown is closed by the power-button driver.
var shutdown = make(chan struct{})

// NewBoard returns the QEMU virt board.
func NewBoard() kernel.Board {
	proc.Kthread("irq", serviceInterrupts)
	// Interrupts are acknowledged in the exception (irqTop), so the boot
	// core can keep IRQs unmasked for good. Secondary cores never unmask.
	arm64.IRQHandler = irqTop
	CPU.EnableInterrupts()
	startSMP()
	nodes, source := devices()
	noteVirtio(nodes)
	// The first FDT_RESERVE bytes hold the boot device tree (entry_arm64.s).
	alloc := dma.New(dma.Region{
		Base: DMA_BASE + FDT_RESERVE,
		Mem:  unsafe.Slice((*byte)(unsafe.Pointer(uintptr(DMA_BASE+FDT_RESERVE))), DMA_SIZE-FDT_RESERVE-PSTORE_SIZE),
	})
	ring := pstore.Open(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(PSTORE_BASE))), PSTORE_SIZE))
	pstoreRing.Store(ring) // runtime output (printk) from now on
	info := BootInfo(source) + "; " + SMPInfo()
	nodes = append(nodes, dev.Node{Name: "cpus", Compatible: "godeep,cpus"})
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
	con.Out = tee{ring, con.Out}
	return kernel.Board{
		Info:       info,
		Console:    con,
		Shutdown:   shutdown,
		PowerOff:   psciPowerOff,
		IRQ:        Bridge,
		DMA:        alloc,
		Devices:    nodes,
		LogLabel:   "GODEEPDATA", // like the Mac: a disk with that FAT32 label gets the log
		Pstore:     ring,
		KexecCheck: kexecCheck,
		Kexec:      kexec,
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

// pstoreRing gets the runtime's output too (printk: panics, throws).
var pstoreRing atomic.Pointer[pstore.Ring]

// tee writes the console to the framebuffer and the UART.
type tee struct{ fb, uart io.Writer }

func (t tee) Write(p []byte) (int, error) {
	t.fb.Write(p)
	return t.uart.Write(p)
}
