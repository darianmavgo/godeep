//go:build tamago && arm64 && !applem1

package virt

import (
	"fmt"
	"unsafe"

	"godeep/drivers/pci"
	virtiommio "godeep/drivers/virtio/mmio"
	"godeep/kernel/dev"
	"godeep/kernel/fdt"
	kmmio "godeep/kernel/mmio"
)

// set by cpuinit (entry_arm64.s)
var bootFDT, bootEL uint64

// boardOwned are nodes the board drives itself (not device-manager drivers).
var boardOwned = map[string]bool{
	"arm,gic-v3": true, "arm,psci-1.0": true, "arm,psci-0.2": true, "arm,psci": true,
	"fixed-clock": true, "qemu,platform": true,
}

// devices returns the device list and a description of where it came from:
// the boot loader's device tree when booted as an Image, else a static list
// matching QEMU virt (ELF boot passes no device tree).
func devices() ([]dev.Node, string) {
	if bootFDT != 0 {
		size := uint32(0)
		hdr := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), 8)
		size = uint32(hdr[4])<<24 | uint32(hdr[5])<<16 | uint32(hdr[6])<<8 | uint32(hdr[7])
		root, err := fdt.Parse(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), size))
		if err == nil {
			if p := root.Find("/psci"); p != nil {
				if m := p.Strings("method"); len(m) > 0 {
					psciMethod = m[0]
				}
			}
			nodes := fromFDT(root)
			model := ""
			if m := root.Strings("model"); len(m) > 0 {
				model = m[0] + ", "
			}
			return nodes, fmt.Sprintf("%sdevice tree from boot loader (%d devices)", model, len(nodes))
		}
		return staticDevices(), "device tree invalid (" + err.Error() + "), static device list"
	}
	return staticDevices(), "no device tree (ELF boot), static device list"
}

func fromFDT(root *fdt.Node) []dev.Node {
	var out []dev.Node
	root.Walk(func(n *fdt.Node) {
		compat := n.Strings("compatible")
		if n.Parent == nil || len(compat) == 0 || boardOwned[compat[0]] || n.Parent.Path == "/cpus" {
			return
		}
		reg := n.Reg()
		if compat[0] == "virtio,mmio" && (len(reg) == 0 || !virtiommio.Present(kmmio.Window{Base: reg[0].Base})) {
			return // empty transport slot
		}
		c := compat[0]
		for _, x := range compat {
			if dev.HasDriver(x) {
				c = x
				break
			}
		}
		node := dev.Node{Name: n.Name, Compatible: c, IRQ: n.GICInterrupts()}
		if c == pci.Compatible {
			if hc, err := pci.ConfigFromFDT(n); err != nil {
				node.Config = err
			} else {
				node.Config = hc
			}
		}
		for _, r := range reg {
			node.Reg = append(node.Reg, dev.Range{Base: r.Base, Size: r.Size})
		}
		out = append(out, node)
	})
	return out
}

func staticDevices() []dev.Node {
	nodes := []dev.Node{
		{Name: "pl011@9000000", Compatible: "arm,pl011", Reg: []dev.Range{{Base: PL011_BASE, Size: 0x1000}}, IRQ: []int{UART0_IRQ}},
		{Name: "pl031@9010000", Compatible: "arm,pl031", Reg: []dev.Range{{Base: PL031_BASE, Size: 0x1000}}, IRQ: []int{RTC_IRQ}},
		{Name: "pl061@9030000", Compatible: "arm,pl061", Reg: []dev.Range{{Base: PL061_BASE, Size: 0x1000}}, IRQ: []int{GPIO_IRQ}},
		{Name: "timer", Compatible: "arm,armv8-timer", IRQ: []int{16 + 13, PTIMER_IRQ, VTIMER_IRQ, 16 + 10}},
		// QEMU virt's PCIe host (highmem ECAM; the 32-bit memory window)
		{Name: "pcie@10000000", Compatible: pci.Compatible, Reg: []dev.Range{{Base: PCIE_ECAM, Size: 0x10000000}},
			Config: pci.HostConfig{ECAM: PCIE_ECAM, Windows: []pci.Window{
				{Space: pci.SpaceMem32, PCI: PCIE_MMIO, CPU: PCIE_MMIO, Size: PCIE_MMIO_SIZE}}}},
	}
	for slot := uint64(0); slot < VIRTIO_MMIO_SLOTS; slot++ {
		base := VIRTIO_MMIO_BASE + slot*0x200
		if virtiommio.Present(kmmio.Window{Base: base}) {
			nodes = append(nodes, dev.Node{Name: fmt.Sprintf("virtio_mmio@%x", base), Compatible: "virtio,mmio",
				Reg: []dev.Range{{Base: base, Size: 0x200}}, IRQ: []int{VIRTIO_IRQ + int(slot)}})
		}
	}
	return nodes
}

// BootInfo describes how this kernel was entered, for the boot banner.
func BootInfo(source string) string {
	el := "EL1"
	if bootEL == 2 {
		el = "EL2, dropped to EL1"
	}
	return fmt.Sprintf("board: qemu-virt; entered at %s; %s; PSCI via %s", el, source, psciMethod)
}
