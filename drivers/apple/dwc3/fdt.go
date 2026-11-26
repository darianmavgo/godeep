package dwc3

import (
	"fmt"

	"godeep/drivers/apple/pmgr"
	"godeep/kernel/fdt"
)

// HPMCompatible is the USB-C port controller.
const HPMCompatible = "apple,cd321x"

// ConfigFromFDT resolves an "apple,t6000-dwc3" node: its registers, the
// Type-C PHY's USB 2 and pipehandler windows ("phys"), its DARTs
// ("iommus"), power domains, and the port controller whose connector's
// endpoint points at it. Keys and Names are left for the board.
func ConfigFromFDT(n *fdt.Node) (Config, error) {
	var c Config
	reg := n.Reg()
	if len(reg) == 0 {
		return c, fmt.Errorf("dwc3: %s has no registers", n.Path)
	}
	c.Core = reg[0].Base

	phys, err := n.Refs("phys", "#phy-cells")
	if err != nil {
		return c, err
	}
	if len(phys) == 0 {
		return c, fmt.Errorf("dwc3: %s has no phys", n.Path)
	}
	phy := phys[0].Node
	if c.USB2PHY, err = named(phy, "usb2phy"); err != nil {
		return c, err
	}
	if c.Pipehandler, err = named(phy, "pipehandler"); err != nil {
		return c, err
	}
	// USB 3: the PHY's core and axi2af windows and m1n1's tunables.
	c.ATC, _ = named(phy, "core")
	c.AXI2AF, _ = named(phy, "axi2af")
	c.Tunables = tunables(phy)

	iommus, err := n.Refs("iommus", "#iommu-cells")
	if err != nil {
		return c, err
	}
	for _, r := range iommus {
		dr := r.Node.Reg()
		if len(dr) == 0 || len(r.Args) != 1 || !r.Node.Compatible("apple,t6000-dart") {
			return c, fmt.Errorf("dwc3: %s: unexpected IOMMU %s", n.Path, r.Node.Path)
		}
		c.DARTs = append(c.DARTs, Stream{Base: dr[0].Base, Stream: int(r.Args[0])})
	}

	seen := map[uint64]bool{}
	for _, x := range append([]*fdt.Node{n, phy}, refNodes(iommus)...) {
		doms, err := pmgr.FromFDT(x)
		if err != nil {
			return c, err
		}
		for _, d := range doms {
			if !seen[d.Addr] {
				seen[d.Addr] = true
				c.Power = append(c.Power, d)
			}
		}
	}

	c.Label = n.Name
	if hpm, conn := portController(n); hpm != nil {
		r := hpm.U32s("reg")
		if len(r) != 1 || hpm.Parent == nil {
			return c, fmt.Errorf("dwc3: port controller %s has no I2C address", hpm.Path)
		}
		c.I2C, c.HPM = hpm.Parent.Name, byte(r[0])
		if l := conn.Strings("label"); len(l) > 0 {
			c.Label = l[0]
		}
	}
	return c, nil
}

func refNodes(rs []fdt.Ref) []*fdt.Node {
	var out []*fdt.Node
	for _, r := range rs {
		out = append(out, r.Node)
	}
	return out
}

// named returns the base of the reg window called name ("reg-names").
func named(n *fdt.Node, name string) (uint64, error) {
	reg := n.Reg()
	for i, s := range n.Strings("reg-names") {
		if s == name && i < len(reg) {
			return reg[i].Base, nil
		}
	}
	return 0, fmt.Errorf("dwc3: %s has no %q registers", n.Path, name)
}

// portController follows the controller's graph endpoints to the CD321x
// whose connector links to it; it returns the CD321x and the connector.
func portController(n *fdt.Node) (hpm, conn *fdt.Node) {
	n.Walk(func(x *fdt.Node) {
		if hpm != nil {
			return
		}
		h := x.U32s("remote-endpoint")
		if len(h) != 1 {
			return
		}
		for t := x.ByPhandle(h[0]); t != nil; t = t.Parent {
			if t.Compatible("usb-c-connector") {
				conn = t
			}
			if t.Compatible(HPMCompatible) {
				hpm = t
				return
			}
		}
	})
	if hpm == nil {
		conn = nil
	}
	return hpm, conn
}

// tunables reads the "apple,tunable-*" triples (offset, mask, value) m1n1
// adds to a Type-C PHY node; nil unless every one USB 3 needs is there.
func tunables(n *fdt.Node) *Tunables {
	read := func(name string) ([]Tunable, bool) {
		v := n.U32s("apple,tunable-" + name)
		if len(v) == 0 || len(v)%3 != 0 {
			return nil, false
		}
		var out []Tunable
		for i := 0; i < len(v); i += 3 {
			if v[i]%4 != 0 {
				return nil, false
			}
			out = append(out, Tunable{Off: v[i], Mask: v[i+1], Val: v[i+2]})
		}
		return out, true
	}
	var t Tunables
	ok := true
	for _, x := range []struct {
		name string
		dst  *[]Tunable
	}{{"axi2af", &t.AXI2AF}, {"common-a", &t.CommonA}, {"common-b", &t.CommonB},
		{"lane0-usb", &t.LaneUSB[0]}, {"lane1-usb", &t.LaneUSB[1]},
		{"lane0-dp", &t.LaneDP[0]}, {"lane1-dp", &t.LaneDP[1]}} {
		v, got := read(x.name)
		ok = ok && got
		*x.dst = v
	}
	if !ok {
		return nil
	}
	return &t
}
