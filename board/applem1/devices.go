//go:build tamago && arm64 && applem1

package applem1

import (
	"errors"
	"fmt"
	"unsafe"

	"godeep/drivers/apple/dwc3"
	"godeep/drivers/apple/i2c"
	"godeep/drivers/apple/pmgr"
	"godeep/drivers/apple/spihid"
	"godeep/drivers/simplefb"
	"godeep/drivers/usb/xhci"
	"godeep/kernel/dev"
	"godeep/kernel/fdt"
)

// framebuffer finds m1n1's "simple-framebuffer" (under /chosen).
func framebuffer(root *fdt.Node) (*simplefb.Framebuffer, error) {
	var node *fdt.Node
	root.Walk(func(n *fdt.Node) {
		for _, c := range n.Strings("compatible") {
			if c == "simple-framebuffer" && node == nil {
				node = n
			}
		}
	})
	if node == nil {
		return nil, errors.New("no simple-framebuffer in the device tree")
	}
	reg := node.Reg()
	w, h, s := node.U32s("width"), node.U32s("height"), node.U32s("stride")
	format := node.Strings("format")
	if len(reg) == 0 || len(w) != 1 || len(h) != 1 || len(s) != 1 || len(format) == 0 {
		return nil, errors.New("incomplete simple-framebuffer node")
	}
	return &simplefb.Framebuffer{
		Pix:    unsafe.Slice((*byte)(unsafe.Pointer(uintptr(reg[0].Base))), reg[0].Size),
		Width:  int(w[0]),
		Height: int(h[0]),
		Stride: int(s[0]),
		Format: simplefb.Format(format[0]),
	}, nil
}

// checkMemory verifies that godeep's fixed region (RAM + DMA) lies inside
// the usable memory m1n1 reports and avoids every reserved range.
func checkMemory(root *fdt.Node) string {
	lo, hi := uint64(RAM_START), uint64(DMA_BASE+DMA_SIZE)
	inside := false
	root.Walk(func(n *fdt.Node) {
		if n.Parent == root && len(n.Name) >= 6 && n.Name[:6] == "memory" {
			for _, r := range n.Reg() {
				if lo >= r.Base && hi <= r.Base+r.Size {
					inside = true
				}
			}
		}
	})
	if !inside {
		return fmt.Sprintf("WARNING: memory %#x-%#x is outside the usable RAM the boot loader reported", lo, hi)
	}
	if rm := root.Find("/reserved-memory"); rm != nil {
		for _, c := range rm.Children {
			for _, r := range c.Reg() {
				if r.Base < hi && r.Base+r.Size > lo {
					return fmt.Sprintf("WARNING: memory overlaps reserved %s", c.Name)
				}
			}
		}
	}
	return "memory layout OK"
}

// fromFDT lists enabled device-tree nodes that have registers.
func fromFDT(root *fdt.Node) []dev.Node {
	var out []dev.Node
	root.Walk(func(n *fdt.Node) {
		compat := n.Strings("compatible")
		if n.Parent == nil || len(compat) == 0 || n.Parent.Path == "/cpus" {
			return
		}
		if st := n.Strings("status"); len(st) > 0 && st[0] != "okay" && st[0] != "ok" {
			return
		}
		reg := n.Reg()
		if len(reg) == 0 {
			return
		}
		node := dev.Node{Name: n.Name, Compatible: compat[0]}
		for _, r := range reg {
			node.Reg = append(node.Reg, dev.Range{Base: r.Base, Size: r.Size})
		}
		out = append(out, node)
	})
	return out
}

// keyboard returns the internal keyboard's device node, configured from the
// tree (or carrying the error that explains why it can't be), with its
// bytes going to keys. ok is false if the tree has no SPI HID transport.
func keyboard(root *fdt.Node, keys chan<- byte) (n dev.Node, ok bool) {
	var t *fdt.Node
	root.Walk(func(x *fdt.Node) {
		if t == nil && x.Compatible(spihid.Compatible) {
			t = x
		}
	})
	if t == nil {
		return n, false
	}
	n = dev.Node{Name: t.Name, Compatible: spihid.Compatible}
	cfg, err := spihid.ConfigFromFDT(t)
	if err != nil {
		n.Config = err
		return n, true
	}
	cfg.Keys = keys
	cfg.Trackpad = true // switch it to multitouch mode: one write to the device
	n.Config = cfg
	n.Reg = []dev.Range{{Base: cfg.SPI, Size: 0x4000}}
	return n, true
}

// withNode replaces the node of the same name in nodes, or appends it.
func withNode(nodes []dev.Node, n dev.Node) []dev.Node {
	for i := range nodes {
		if nodes[i].Name == n.Name {
			nodes[i] = n
			return nodes
		}
	}
	return append(nodes, n)
}

// usbPorts configures the USB-C controllers (dwc3) and the I2C buses their
// port controllers are on, in place in nodes. Keyboards type into keys.
func usbPorts(root *fdt.Node, nodes []dev.Node, keys chan<- byte) {
	names := &xhci.Names{} // usbdisk<N>, usbnet<N>: shared by every port
	byName := map[string]*fdt.Node{}
	root.Walk(func(n *fdt.Node) { byName[n.Name] = n })
	for i := range nodes {
		n := &nodes[i]
		fn := byName[n.Name]
		if fn == nil {
			continue
		}
		switch n.Compatible {
		case i2c.Compatible:
			doms, err := pmgr.FromFDT(fn)
			if err != nil {
				n.Config = err
				continue
			}
			n.Config = i2c.Config{Power: doms}
		case dwc3.Compatible:
			c, err := dwc3.ConfigFromFDT(fn)
			if err != nil {
				n.Config = err // lsdev shows why
				continue
			}
			c.Keys, c.Names = keys, names
			if c.I2C != "" {
				n.Needs = append(n.Needs, c.I2C)
			}
			n.Config = c
		}
	}
}
