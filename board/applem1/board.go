//go:build tamago && arm64 && applem1

package applem1

import (
	"fmt"
	"os"
	"slices"
	"sync/atomic"
	"unsafe"

	"github.com/usbarmory/tamago/arm64"

	"godeep/drivers/arm/pl011"
	"godeep/drivers/simplefb"
	"godeep/kernel"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/fdt"
	"godeep/kernel/pstore"
)

// set by cpuinit (entry_arm64.s)
var bootFDT, bootEL, bootE2H uint64

// Autorun is typed into the shell at boot, before anything from the
// keyboard. The last lines show, after a few idle seconds, the idle loop's
// wake-up rate, the watchdog's countdown, and how far the keyboard driver
// got (hid-transport@0), so a boot that can't type still reports.
var Autorun = "lsdev\rps\rmem\ruptime\rsleep 5\rlsdev idle watchdog hid usb nvme smc rtc kbdlight\r"

// Desktop follows Autorun: terminal windows on the framebuffer
// (programs/sh/desktop.go), as on Parallels. Super+Q leaves it for the
// plain console; godeep.nodesktop in bootargs skips it. `enable` lists
// what the board left off at boot (smp, agx).
var Desktop = "enable\rdesktop\r"

// Typeahead from the keyboard beyond this is dropped (counted in lsdev).
const keyBuffer = 256

var console *simplefb.Console

//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	if console != nil {
		console.Write([]byte{c})
	}
	if uartBase != 0 {
		uartPut(c)
	}
	pstoreRing.Load().WriteByte(c) // nil until NewBoard: no-op
}

// pstoreRing records the console and runtime output across resets.
var pstoreRing atomic.Pointer[pstore.Ring]

// NewBoard brings up the framebuffer console and describes the machine.
func NewBoard() kernel.Board {
	stage(StageRuntime)
	prevLog := prevBootLog()
	ring := pstore.Open(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(PSTORE_BASE+BOOTLOG_SIZE))), PSTORE_SIZE-BOOTLOG_SIZE))
	pstoreRing.Store(ring)
	var root *fdt.Node
	info := []string{}
	if bootFDT != 0 {
		hdr := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), 8)
		size := uint32(hdr[4])<<24 | uint32(hdr[5])<<16 | uint32(hdr[6])<<8 | uint32(hdr[7])
		r, err := fdt.Parse(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), size))
		if err == nil {
			root = r
			blog("fdt: parsed\n")
		} else {
			blog("fdt: " + err.Error() + "\n")
		}
	}

	var out writer = discard{}
	if root != nil {
		if fb, err := framebuffer(root); err == nil {
			if c, err := simplefb.NewConsole(fb, 0); err == nil {
				// the console clears the screen: show the boot log so far
				// on it (blog prints there from now on)
				fmt.Fprintf(c, "boot log (this boot):\n%s", curBootLog())
				console, out = c, c
				info = append(info, fmt.Sprintf("framebuffer %dx%d %s", fb.Width, fb.Height, fb.Format))
			} else {
				blog("console: " + err.Error() + "\n")
			}
		} else {
			blog("console: " + err.Error() + "\n")
		}
	}
	if console != nil {
		blog("console: up\n")
		// From now on a fault in a goroutine panics in it (its process
		// dies, the kernel stays: docs/UnderstandingGoRoutineFailure.md);
		// before, the fault screen (lateFault) was the only report.
		arm64.FaultPanics = true
	}

	if uartReady {
		out = tee{out, uartWriter{}}
	}
	out = tee{out, ring}

	el := "EL1"
	if bootEL == 2 {
		el = "EL2, dropped to EL1"
		if bootE2H == 1 {
			el = "EL2 (VHE, E2H=1), dropped to EL1"
		}
	}
	model := "unknown model"
	nodes := 0
	devices := []string{}
	if root != nil {
		if m := root.Strings("model"); len(m) > 0 {
			model = m[0]
		}
		nodes = len(fromFDT(root))
		devices = append(devices, checkMemory(root), bootEntropy(root))
		bootRoot = root
		blog("smp: start\n")
		smpDefault(root)
		blog("smp: " + SMPInfo() + "\n")
		devices = append(devices, SMPInfo())
	} else {
		devices = append(devices, "no device tree from the boot loader")
	}

	// One input stream: autorun first, then the keyboard driver's bytes (and
	// on QEMU, the serial port's).
	// Never closed.
	autorun := Autorun
	if root == nil || !bootArg(root, "godeep.nodesktop") {
		autorun += Desktop
	}
	in := make(chan byte, len(autorun)+keyBuffer)
	for i := 0; i < len(autorun); i++ {
		in <- autorun[i]
	}

	b := kernel.Board{
		Info: fmt.Sprintf("board: %s; entered at %s; %d devices from the device tree; %s; %s",
			model, el, nodes, joinInfo(info), joinInfo(devices)),
		Console: kernel.Console{In: in, Out: out, Echo: true},
		NetWait: -1, // until a USB Ethernet adapter is plugged in
		// The kernel log goes to godeep/log.txt on GODEEPDATA (once
		// ssdWrites is on), or on any USB stick formatted FAT32 with that label.
		LogLabel: "GODEEPDATA",
		// After halt has stopped every driver (the SSD shut its firmware
		// down): goos.Exit powers off through the SMC, or keeps the screen.
		PowerOff: func() { os.Exit(0) },
		Pstore:   ring,
	}
	if console != nil && !uartReady {
		b.Console.Size = console.Size // the screen is the only terminal
	}
	if console != nil {
		// The trackpad (and any mouse) drives the console: pointer, two-finger
		// scroll through history, click-drag to select, right click to paste.
		b.Console.Input = &simplefb.Mouse{C: console, Paste: simplefb.PasteTo(in)}
	}
	b.DMA = dma.New(dma.Region{
		Base: DMA_BASE + FDT_RESERVE,
		Mem:  unsafe.Slice((*byte)(unsafe.Pointer(uintptr(DMA_BASE+FDT_RESERVE))), DMA_SIZE-FDT_RESERVE-PSTORE_SIZE),
	})
	if root != nil {
		b.Devices = fromFDT(root)
		if kbd, ok := keyboard(root, in); ok {
			b.Devices = withNode(b.Devices, kbd)
		}
		b.Devices = append(b.Devices, configureNodes(root, b.Devices, in)...)
		if agxOff != nil { // off until `enable agx` (features.go)
			b.Devices = slices.DeleteFunc(b.Devices, func(n dev.Node) bool { return n.Name == agxOff.Name })
		}
		var irqInfo string
		b.IRQ, irqInfo = interrupts(root)
		b.Info += "; " + irqInfo
		for i := range b.Devices {
			if b.Devices[i].Compatible == pl011.Compatible {
				b.Devices[i].Config = (chan<- byte)(in) // serial input (QEMU)
			}
		}
	}
	b.Devices = append(b.Devices, dev.Node{Name: "idle", Compatible: idleCompatible},
		dev.Node{Name: "features", Compatible: featuresCompatible})
	if console != nil {
		b.Devices = append(b.Devices, dev.Node{Name: "fbcon", Compatible: simplefb.DeviceCompatible, Config: console})
	}
	blog("board: ")
	blogDec(uint64(len(b.Devices)))
	blog(" devices, starting the kernel\n")
	b.BootLog, b.PrevBootLog = curBootLog(), prevLog
	return b
}

type writer interface{ Write([]byte) (int, error) }

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// tee writes to the framebuffer console and the serial mirror.
type tee struct{ a, b writer }

func (t tee) Write(p []byte) (int, error) {
	t.b.Write(p)
	return t.a.Write(p)
}

type uartWriter struct{}

func (uartWriter) Write(p []byte) (int, error) {
	for _, c := range p {
		uartPut(c)
	}
	return len(p), nil
}

func joinInfo(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += "; "
		}
		out += x
	}
	return out
}
