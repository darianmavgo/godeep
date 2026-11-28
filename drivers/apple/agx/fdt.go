package agx

import (
	"fmt"

	"godeep/drivers/apple/aic"
	"godeep/drivers/apple/pmgr"
	"godeep/kernel/fdt"
)

// Region is a reserved-memory region.
type Region struct{ Base, Size uint64 }

// Config is what the driver needs from the device tree.
type Config struct {
	ASC, SGX    uint64 // "asc" (coprocessor control) and "sgx" (GPU) windows
	Mailbox     uint64
	MailboxIRQs []int
	Power       []pmgr.Domain // parents first
	// Regions iBoot set up and m1n1 copied into the tree.
	Handoff, TTBs, PageTables Region
	// Firmware is the version iBoot loaded; Compat the structure layout
	// it speaks ("apple,firmware-version", "apple,firmware-compat").
	Firmware, Compat [3]uint32
	// Pwr is the power configuration (operating points, control loops).
	Pwr PwrConfig
	// Calibration blobs, if m1n1 provided them ("hw-cal-a", "hw-cal-b",
	// "globals"): what a known-good driver built, to compare ours with.
	CalA, CalB, CalGlobals Region
}

// ConfigFromFDT resolves an "apple,agx-t6000" node.
func ConfigFromFDT(n *fdt.Node) (Config, error) {
	var c Config
	reg := n.Reg()
	for i, name := range n.Strings("reg-names") {
		if i >= len(reg) {
			break
		}
		switch name {
		case "asc":
			c.ASC = reg[i].Base
		case "sgx":
			c.SGX = reg[i].Base
		}
	}
	if c.ASC == 0 || c.SGX == 0 {
		return c, fmt.Errorf("agx: %s lacks asc/sgx registers", n.Path)
	}
	mb, err := n.Refs("mboxes", "#mbox-cells")
	if err != nil || len(mb) == 0 || len(mb[0].Node.Reg()) == 0 {
		return c, fmt.Errorf("agx: %s: no mailbox (%v)", n.Path, err)
	}
	c.Mailbox = mb[0].Node.Reg()[0].Base
	c.MailboxIRQs, _ = aic.IRQs(mb[0].Node)
	seen := map[uint64]bool{}
	for _, x := range []*fdt.Node{mb[0].Node, n} {
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

	regions, err := n.Refs("memory-region", "")
	if err != nil {
		return c, fmt.Errorf("agx: %s: %v", n.Path, err)
	}
	for i, name := range n.Strings("memory-region-names") {
		if i >= len(regions) {
			break
		}
		r := regions[i].Node
		if st := r.Strings("status"); len(st) > 0 && st[0] != "okay" && st[0] != "ok" {
			continue // m1n1 didn't fill it in
		}
		rr := r.Reg()
		if len(rr) == 0 {
			continue
		}
		reg := Region{rr[0].Base, rr[0].Size}
		switch name {
		case "handoff":
			c.Handoff = reg
		case "ttbs":
			c.TTBs = reg
		case "pagetables":
			c.PageTables = reg
		}
	}
	switch {
	case c.Handoff.Size < HandoffSize:
		return c, fmt.Errorf("agx: handoff region missing or too small (%#x): m1n1 didn't set up the GPU", c.Handoff.Size)
	case c.TTBs.Size < NumCtx*slotSize:
		return c, fmt.Errorf("agx: ttbs region missing or too small (%#x)", c.TTBs.Size)
	case c.PageTables.Size < PageSize || c.PageTables.Base&(PageSize-1) != 0:
		return c, fmt.Errorf("agx: pagetables region missing or unaligned (%#x+%#x)", c.PageTables.Base, c.PageTables.Size)
	}

	v, cv := n.U32s("apple,firmware-version"), n.U32s("apple,firmware-compat")
	if len(v) != 3 || len(cv) != 3 {
		return c, fmt.Errorf("agx: %s: no firmware version (m1n1 sets it)", n.Path)
	}
	c.Firmware, c.Compat = [3]uint32(v), [3]uint32(cv)
	if c.Pwr, err = PwrFromFDT(n, T6000); err != nil {
		return c, err
	}
	c.CalA, _ = blobRegion(n, "hw-cal-a")
	c.CalB, _ = blobRegion(n, "hw-cal-b")
	c.CalGlobals, _ = blobRegion(n, "globals")
	return c, nil
}
