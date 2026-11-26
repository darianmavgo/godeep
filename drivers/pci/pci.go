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
// Bridges (PCIe root ports, switches) are followed depth first, as Linux
// does: each gets the next bus number, its subordinate number once its
// subtree is known, and a memory window around its children's BARs (1 MB
// granules). The I/O and prefetchable windows stay closed; every BAR goes
// in the memory window, which works for 32- and 64-bit BARs alike. I/O BARs
// are skipped: PCIe devices don't need them.
package pci

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"godeep/drivers/arm/smmuv3"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
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

	// Type 1 (bridge) header.
	BUS_NUMBERS = 0x18 // primary, secondary, subordinate, latency
	IO_BASE     = 0x1c // I/O base and limit (low 16), secondary status (high 16)
	MEM_BASE    = 0x20 // memory base (low 16), limit (high 16): address bits 31:20
	PREF_BASE   = 0x24
	PREF_UPPER  = 0x28 // and PREF_LIMIT_UPPER at 0x2c
	BRIDGE_CTRL = 0x3c
	bridgeAlign = 1 << 20
	maxBuses    = 256

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
	SMMU    uint64 // an Arm SMMUv3 in front of the bus ("iommu-map"); 0: none
}

// IOVABase is where the DMA region appears to devices behind an IOMMU.
const IOVABase = 0x1000_0000

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
	Secondary      int    // bridges: the bus behind it
	Subordinate    int    // bridges: the last bus in its subtree
	BARs           [6]BAR // index = BAR number; Size 0: absent or not assigned
	Name           string // e.g. "nvme0"
	// DMA is the allocator for this function's DMA buffers when the bus
	// is behind an IOMMU (buffers' Bus addresses are what it sees); nil:
	// the board's, untranslated.
	DMA *dma.Allocator

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
		return s + fmt.Sprintf(" bridge to buses %d-%d", f.Secondary, f.Subordinate)
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

// Scan enumerates the buses behind cfg (bus 0, then every bridge's) and
// assigns memory BARs from the windows. Functions are returned in the order
// they were found (depth first).
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
	s := &scanner{cfg: cfg, mem32: mem32, mem64: mem64, counts: map[string]int{}}
	_, err := s.bus(0)
	return s.out, err
}

type scanner struct {
	cfg          mmio.Regs
	mem32, mem64 *allocator
	counts       map[string]int
	out          []*Function
	lastBus      int
}

// window is the allocator bridge windows come from: the one BARs use.
func (s *scanner) window() *allocator {
	if s.mem32 != nil {
		return s.mem32
	}
	return s.mem64
}

// bus scans one bus and, depth first, the buses behind its bridges. It
// returns the last bus number used.
func (s *scanner) bus(b int) (int, error) {
	cfg := s.cfg
	for d := 0; d < 32; d++ {
		for fn := 0; fn < 8; fn++ {
			off := uint64(b)<<20 | uint64(d)<<15 | uint64(fn)<<12
			id := cfg.Read32(off + VENDOR_ID)
			if id&0xffff == 0xffff {
				if fn == 0 {
					break // no device in this slot
				}
				continue
			}
			f := &Function{Bus: b, Dev: d, Fn: fn, Vendor: uint16(id), Device: uint16(id >> 16),
				Class: cfg.Read32(off+CLASS) >> 8, cfg: cfg, off: off}
			ht := cfg.Read32(off+HEADER_TYPE) >> 16 & 0xff
			f.Bridge = ht&0x7f == 1
			switch ht & 0x7f {
			case 0:
				if err := f.assign(s.mem32, s.mem64, 6); err != nil {
					return s.lastBus, err
				}
			case 1:
				if err := f.assign(s.mem32, s.mem64, 2); err != nil {
					return s.lastBus, err
				}
			}
			short := shortName(f.Class)
			if f.Bridge {
				short = "bridge"
			}
			f.Name = fmt.Sprintf("%s%d", short, s.counts[short])
			s.counts[short]++
			s.out = append(s.out, f)
			if f.Bridge {
				if err := s.bridge(f); err != nil {
					return s.lastBus, err
				}
			}
			if fn == 0 && ht&0x80 == 0 {
				break // single-function device
			}
		}
	}
	return s.lastBus, nil
}

// bridge numbers the bus behind f, scans it, and opens f's memory window
// around what its subtree got.
func (s *scanner) bridge(f *Function) error {
	if s.lastBus+1 >= maxBuses {
		return fmt.Errorf("pci: %s: out of bus numbers", f)
	}
	s.lastBus++
	f.Secondary = s.lastBus
	// While scanning, the bridge forwards every bus above its secondary.
	busReg := func(sub int) uint32 {
		return f.ConfigRead32(BUS_NUMBERS)&0xff000000 | uint32(sub)<<16 | uint32(f.Secondary)<<8 | uint32(f.Bus)
	}
	f.ConfigWrite32(BUS_NUMBERS, busReg(0xff))
	f.ConfigWrite32(IO_BASE, f.ConfigRead32(IO_BASE)&0xffff0000|0x00f0) // I/O closed (base > limit)
	f.ConfigWrite32(PREF_BASE, 0x0000fff0)                              // prefetchable closed
	f.ConfigWrite32(PREF_UPPER, 0)
	f.ConfigWrite32(PREF_UPPER+4, 0)

	a := s.window()
	a.next = (a.next + bridgeAlign - 1) &^ (bridgeAlign - 1)
	start := a.next
	if _, err := s.bus(f.Secondary); err != nil {
		return err
	}
	f.Subordinate = s.lastBus
	f.ConfigWrite32(BUS_NUMBERS, busReg(f.Subordinate))

	end := (a.next + bridgeAlign - 1) &^ (bridgeAlign - 1)
	if end > start { // children have BARs: forward [start, end)
		a.next = end
		f.ConfigWrite32(MEM_BASE, uint32(end-1)&0xfff00000|uint32(start)>>16&0xfff0)
	} else {
		f.ConfigWrite32(MEM_BASE, 0x0000fff0) // closed
	}
	f.ConfigWrite32(COMMAND, f.ConfigRead32(COMMAND)&0xffff|CMD_MEMORY|CMD_BUS_MASTER)
	return nil
}

// assign sizes and places the function's memory BARs, then enables memory
// decoding and bus mastering.
func (f *Function) assign(mem32, mem64 *allocator, nbars int) error {
	// Decoding off while BARs hold sizing patterns.
	f.ConfigWrite32(COMMAND, f.ConfigRead32(COMMAND)&0xffff&^(CMD_MEMORY|CMD_BUS_MASTER))
	assigned := false
	for i := 0; i < nbars; i++ {
		reg := BAR0 + uint64(4*i)
		orig := f.ConfigRead32(reg)
		if orig&BAR_IO != 0 {
			continue
		}
		is64 := orig&(3<<1) == BAR_TYPE_64
		if is64 && i == nbars-1 {
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
		var iommu *smmuv3.SMMU
		var translated *dma.Allocator
		if hc.SMMU != 0 && d.DMA != nil {
			var err error
			if iommu, err = smmuv3.Setup(regs(hc.SMMU), d.DMA, IOVABase, 12); err != nil {
				return err
			}
			translated = d.DMA.Translated(IOVABase)
		}
		fns, err := Scan(regs(hc.ECAM), hc.Windows)
		if err != nil {
			return err
		}
		list := &lister{iommu: iommu}
		for _, f := range fns {
			f.DMA = translated
			list.lines = append(list.lines, f.Name+" "+f.String())
			if f.Bridge || f.Class>>8 == 0x0600 || f.Class>>8 == 0x0604 { // bridges and the host bridge itself
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
		sort.Strings(list.lines)
		d.Ready(list)
		<-ctx.Done()
		return nil
	}}
}

type lister struct {
	lines []string
	iommu *smmuv3.SMMU
}

func (l *lister) Devices() []string {
	if l.iommu == nil {
		return l.lines
	}
	out := append([]string{l.iommu.Info()}, l.lines...)
	for _, e := range l.iommu.Events() {
		out = append(out, "IOMMU fault: "+e)
	}
	return out
}
