// Package pci enumerates a PCI Express bus behind an ECAM host bridge
// ("pci-host-ecam-generic": QEMU virt; Apple's root complex later) and
// starts a driver per function.
//
// With a -kernel boot (QEMU) or m1n1 there is no firmware that assigns
// BARs, so the host driver does it: it sizes every memory BAR, places it in
// the bridge's memory window (from the device tree's "ranges") and enables
// memory decoding and bus mastering. Each function becomes a device named
// after its class ("nvme0", "xhci0", "pci0" ...) with compatible
// "pciclass,CCSSPP" (class, subclass, programming interface: the Linux
// convention), so drivers bind by class. Its Config is the *Function.
//
// Scope: bus 0 only (QEMU virt puts devices there). Bridges are listed
// but not followed. I/O BARs are skipped: PCIe devices don't need them.
package pci

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// Compatible is the host bridge's device-tree string.
const Compatible = "pci-host-ecam-generic"

// Config-space registers.
const (
	VENDOR_ID   = 0x00 // vendor (low 16), device (high 16)
	COMMAND     = 0x04 // command (low 16), status (high 16, write-1-to-clear)
	CLASS       = 0x08 // revision (low 8), class code (high 24)
	HEADER_TYPE = 0x0c // byte 2
	BAR0        = 0x10

	CMD_MEMORY     = 1 << 1
	CMD_BUS_MASTER = 1 << 2

	BAR_IO       = 1 << 0
	BAR_TYPE_64  = 2 << 1
	BAR_PREFETCH = 1 << 3
)

// Space is a window's address space.
type Space int

const (
	SpaceIO Space = iota + 1
	SpaceMem32
	SpaceMem64
)

// Window maps a range of PCI bus addresses to CPU addresses.
type Window struct {
	Space    Space
	Prefetch bool
	PCI, CPU uint64
	Size     uint64
}

// HostConfig describes an ECAM host bridge (ConfigFromFDT).
type HostConfig struct {
	ECAM    uint64 // config space: bus<<20 | dev<<15 | fn<<12 | register
	Windows []Window
}

// BAR is one assigned memory BAR.
type BAR struct {
	CPU, PCI, Size uint64
	Is64, Prefetch bool
}

// Function is one PCI function.
type Function struct {
	Bus, Dev, Fn   int
	Vendor, Device uint16
	Class          uint32 // class<<16 | subclass<<8 | prog-if
	Bridge         bool
	BARs           [6]BAR // index = BAR number; Size 0: absent or not assigned
	Name           string // e.g. "nvme0"

	cfg mmio.Regs
	off uint64
}

// ConfigRead32 reads a config-space register of the function.
func (f *Function) ConfigRead32(reg uint64) uint32 { return f.cfg.Read32(f.off + reg) }

// ConfigWrite32 writes a config-space register of the function.
func (f *Function) ConfigWrite32(reg uint64, v uint32) { f.cfg.Write32(f.off+reg, v) }

func (f *Function) String() string {
	s := fmt.Sprintf("%02x:%02x.%x %04x:%04x class %06x", f.Bus, f.Dev, f.Fn, f.Vendor, f.Device, f.Class)
	if f.Bridge {
		return s + " bridge (not scanned)"
	}
	for i, b := range f.BARs {
		if b.Size != 0 {
			s += fmt.Sprintf(" BAR%d %#x+%#x", i, b.CPU, b.Size)
		}
	}
	return s
}

// Compatible returns the function's compatible string.
func (f *Function) Compatible() string { return fmt.Sprintf("pciclass,%06x", f.Class) }

// shortNames name devices by class.
var shortNames = map[uint32]string{
	0x010802: "nvme",
	0x0c0330: "xhci",
	0x080500: "mmc", 0x080501: "mmc",
	0x060000: "host",
}

func shortName(class uint32) string {
	if n, ok := shortNames[class]; ok {
		return n
	}
	if class>>16 == 0x02 {
		return "net"
	}
	return "pci"
}

// allocator hands out addresses from one window.
type allocator struct {
	w    Window
	next uint64 // PCI address
}

func (a *allocator) alloc(size uint64) (uint64, bool) {
	addr := (a.next + size - 1) &^ (size - 1) // BARs are naturally aligned
	if addr+size > a.w.PCI+a.w.Size || addr+size < addr {
		return 0, false
	}
	a.next = addr + size
	return addr, true
}

// Scan enumerates bus 0 behind cfg and assigns memory BARs from the
// windows. Functions are returned in bus/device/function order.
func Scan(cfg mmio.Regs, windows []Window) ([]*Function, error) {
	var mem32, mem64 *allocator
	for _, w := range windows {
		w := w
		switch {
		case w.Space == SpaceMem32 && mem32 == nil:
			mem32 = &allocator{w: w, next: w.PCI}
		case w.Space == SpaceMem64 && mem64 == nil:
			mem64 = &allocator{w: w, next: w.PCI}
		}
	}
	if mem32 == nil && mem64 == nil {
		return nil, errors.New("pci: the host bridge has no memory window")
	}

	var out []*Function
	counts := map[string]int{}
	for d := 0; d < 32; d++ {
		for fn := 0; fn < 8; fn++ {
			off := uint64(d)<<15 | uint64(fn)<<12
			id := cfg.Read32(off + VENDOR_ID)
			if id&0xffff == 0xffff {
				if fn == 0 {
					break // no device in this slot
				}
				continue
			}
			f := &Function{Dev: d, Fn: fn, Vendor: uint16(id), Device: uint16(id >> 16),
				Class: cfg.Read32(off+CLASS) >> 8, cfg: cfg, off: off}
			ht := cfg.Read32(off+HEADER_TYPE) >> 16 & 0xff
			f.Bridge = ht&0x7f == 1
			if !f.Bridge && ht&0x7f == 0 {
				if err := f.assign(mem32, mem64); err != nil {
					return out, err
				}
			}
			short := shortName(f.Class)
			f.Name = fmt.Sprintf("%s%d", short, counts[short])
			counts[short]++
			out = append(out, f)
			if fn == 0 && ht&0x80 == 0 {
				break // single-function device
			}
		}
	}
	return out, nil
}

// assign sizes and places the function's memory BARs, then enables memory
// decoding and bus mastering.
func (f *Function) assign(mem32, mem64 *allocator) error {
	// Decoding off while BARs hold sizing patterns.
	f.ConfigWrite32(COMMAND, f.ConfigRead32(COMMAND)&0xffff&^(CMD_MEMORY|CMD_BUS_MASTER))
	assigned := false
	for i := 0; i < 6; i++ {
		reg := BAR0 + uint64(4*i)
		orig := f.ConfigRead32(reg)
		if orig&BAR_IO != 0 {
			continue
		}
		is64 := orig&(3<<1) == BAR_TYPE_64
		if is64 && i == 5 {
			return fmt.Errorf("pci: %s: 64-bit BAR5 has no upper half", f)
		}
		f.ConfigWrite32(reg, 0xffffffff)
		lo := uint64(f.ConfigRead32(reg) &^ 0xf)
		mask := lo | 0xffffffff_00000000
		if is64 {
			f.ConfigWrite32(reg+4, 0xffffffff)
			mask = lo | uint64(f.ConfigRead32(reg+4))<<32
		}
		size := ^mask + 1
		if !is64 && lo == 0 || is64 && mask == 0 {
			f.ConfigWrite32(reg, orig) // unimplemented BAR
			if is64 {
				i++
			}
			continue
		}
		var a *allocator
		switch {
		case mem32 != nil:
			a = mem32 // 64-bit BARs fit below 4 GB too, and every window is mapped
		case is64:
			a = mem64
		}
		if a == nil {
			return fmt.Errorf("pci: %s BAR%d: no window for it", f, i)
		}
		addr, ok := a.alloc(size)
		if !ok {
			return fmt.Errorf("pci: %s BAR%d: %#x bytes don't fit in the window", f, i, size)
		}
		f.ConfigWrite32(reg, uint32(addr))
		if is64 {
			f.ConfigWrite32(reg+4, uint32(addr>>32))
		}
		f.BARs[i] = BAR{CPU: a.w.CPU + (addr - a.w.PCI), PCI: addr, Size: size, Is64: is64, Prefetch: orig&BAR_PREFETCH != 0}
		assigned = true
		if is64 {
			i++
		}
	}
	if assigned {
		f.ConfigWrite32(COMMAND, f.ConfigRead32(COMMAND)&0xffff|CMD_MEMORY|CMD_BUS_MASTER)
	}
	return nil
}

// Driver returns the host bridge driver. The node's Config is a HostConfig
// (or the error resolving it); regs maps a physical address to registers.
func Driver(regs func(base uint64) mmio.Regs) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		var hc HostConfig
		switch c := d.Config.(type) {
		case error:
			return c
		case HostConfig:
			hc = c
		default:
			return fmt.Errorf("pci: no configuration from the board (%T)", d.Config)
		}
		fns, err := Scan(regs(hc.ECAM), hc.Windows)
		if err != nil {
			return err
		}
		list := make(lister, 0, len(fns))
		for _, f := range fns {
			list = append(list, f.Name+" "+f.String())
			if f.Bridge || f.Class>>8 == 0x0600 { // bridges and the host bridge itself
				continue
			}
			n := dev.Node{Name: f.Name, Compatible: f.Compatible(), Config: f}
			for _, b := range f.BARs {
				if b.Size != 0 {
					n.Reg = append(n.Reg, dev.Range{Base: b.CPU, Size: b.Size})
				}
			}
			d.Add(n)
		}
		sort.Strings(list)
		d.Ready(list)
		<-ctx.Done()
		return nil
	}}
}

type lister []string

func (l lister) Devices() []string { return l }
