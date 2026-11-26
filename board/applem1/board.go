//go:build tamago && arm64 && applem1

package applem1

import (
	"fmt"
	"unsafe"

	"godeep/drivers/simplefb"
	"godeep/kernel"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/fdt"
)

// set by cpuinit (entry_arm64.s)
var bootFDT, bootEL, bootE2H uint64

// Autorun is typed into the shell at boot, before anything from the
// keyboard. The last lines show, after a few idle seconds, the idle loop's
// wake-up rate, the watchdog's countdown, and how far the keyboard driver
// got (hid-transport@0), so a boot that can't type still reports.
var Autorun = "lsdev\rps\rmem\ruptime\rsleep 5\rlsdev idle watchdog hid\r"

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
}

// NewBoard brings up the framebuffer console and describes the machine.
func NewBoard() kernel.Board {
	stage(StageRuntime)
	var root *fdt.Node
	info := []string{}
	if bootFDT != 0 {
		hdr := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), 8)
		size := uint32(hdr[4])<<24 | uint32(hdr[5])<<16 | uint32(hdr[6])<<8 | uint32(hdr[7])
		r, err := fdt.Parse(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), size))
		if err == nil {
			root = r
		}
	}

	var out writer = discard{}
	if root != nil {
		if fb, err := framebuffer(root); err == nil {
			if c, err := simplefb.NewConsole(fb, 0); err == nil {
				console, out = c, c
				info = append(info, fmt.Sprintf("framebuffer %dx%d %s", fb.Width, fb.Height, fb.Format))
			}
		}
	}

	if uartReady {
		out = tee{out, uartWriter{}}
	}

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
		devices = append(devices, checkMemory(root))
	} else {
		devices = append(devices, "no device tree from the boot loader")
	}

	// One input stream: autorun first, then the keyboard driver's bytes.
	// Never closed.
	in := make(chan byte, len(Autorun)+keyBuffer)
	for i := 0; i < len(Autorun); i++ {
		in <- Autorun[i]
	}

	b := kernel.Board{
		Info: fmt.Sprintf("board: %s; entered at %s; %d devices from the device tree; %s; %s",
			model, el, nodes, joinInfo(info), joinInfo(devices)),
		Console: kernel.Console{In: in, Out: out, Echo: true},
		DMA: dma.New(dma.Region{
			Base: DMA_BASE + FDT_RESERVE,
			Mem:  unsafe.Slice((*byte)(unsafe.Pointer(uintptr(DMA_BASE+FDT_RESERVE))), DMA_SIZE-FDT_RESERVE),
		}),
	}
	if root != nil {
		b.Devices = fromFDT(root)
		if kbd, ok := keyboard(root, in); ok {
			b.Devices = withNode(b.Devices, kbd)
		}
	}
	b.Devices = append(b.Devices, dev.Node{Name: "idle", Compatible: idleCompatible})
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
