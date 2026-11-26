package pci

import (
	"errors"
	"fmt"

	"godeep/kernel/fdt"
)

// ConfigFromFDT reads a host bridge node: "reg" is the ECAM window and
// "ranges" maps PCI address windows to CPU addresses. Each "ranges" entry
// is a 3-cell PCI address (phys.hi: bits 24-25 the space, bit 30
// prefetchable; then the 64-bit address), the parent's #address-cells of
// CPU address, and the node's #size-cells of size.
func ConfigFromFDT(n *fdt.Node) (HostConfig, error) {
	var c HostConfig
	reg := n.Reg()
	if len(reg) == 0 {
		return c, fmt.Errorf("pci: %s has no ECAM window", n.Path)
	}
	c.ECAM = reg[0].Base
	if br := n.U32s("bus-range"); len(br) == 2 && br[0] != 0 {
		return c, fmt.Errorf("pci: %s: buses start at %d; godeep numbers them from 0", n.Path, br[0])
	}
	pac := cells(n.Parent, "#address-cells", 2)
	sc := cells(n, "#size-cells", 2)
	if cells(n, "#address-cells", 3) != 3 {
		return c, fmt.Errorf("pci: %s: #address-cells isn't 3", n.Path)
	}
	v := n.U32s("ranges")
	step := 3 + pac + sc
	if len(v) == 0 || len(v)%step != 0 {
		return c, errors.New("pci: missing or malformed ranges")
	}
	join := func(c []uint32) uint64 {
		var x uint64
		for _, w := range c {
			x = x<<32 | uint64(w)
		}
		return x
	}
	// An SMMUv3 in front of the bus: iommu-map = <rid-base iommu sid-base length>.
	if m := n.U32s("iommu-map"); len(m) >= 4 && m[0] == 0 && m[2] == 0 {
		if s := n.ByPhandle(m[1]); s != nil && s.Compatible("arm,smmu-v3") && len(s.Reg()) > 0 {
			c.SMMU = s.Reg()[0].Base
		}
	}
	for i := 0; i < len(v); i += step {
		hi := v[i]
		w := Window{
			Space:    Space(hi >> 24 & 3),
			Prefetch: hi&(1<<30) != 0,
			PCI:      join(v[i+1 : i+3]),
			CPU:      n.Parent.TranslateAddr(join(v[i+3 : i+3+pac])),
			Size:     join(v[i+3+pac : i+step]),
		}
		if w.Space != 0 {
			c.Windows = append(c.Windows, w)
		}
	}
	return c, nil
}

func cells(n *fdt.Node, prop string, def int) int {
	if n == nil {
		return def
	}
	if v := n.U32s(prop); len(v) == 1 {
		return int(v[0])
	}
	return def
}
