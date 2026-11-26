// Package pmgr turns on Apple Silicon power domains ("apple,pmgr-pwrstate"),
// after Linux's apple-pmgr-pwrstate.c and U-Boot's apple-pmgr.c.
//
// Each domain is one 32-bit register inside a PMGR block. Software writes a
// target power state; the hardware reports the state it reached. Domains
// form a tree ("power-domains" points at the parents) and a child can only
// be on if its parents are. godeep only ever turns domains on.
package pmgr

import (
	"context"
	"errors"
	"fmt"
	"time"

	"godeep/kernel/fdt"
	"godeep/kernel/mmio"
)

// Register bits.
const (
	RESET        = 1 << 31
	DEV_DISABLE  = 1 << 10
	WAS_CLKGATED = 1 << 9 // write 1 to clear
	WAS_PWRGATED = 1 << 8 // write 1 to clear
	PS_ACTUAL    = 0xf << 4
	PS_TARGET    = 0xf

	ACTIVE = 0xf
)

// Timeout for a domain to reach the target state.
const Timeout = 100 * time.Millisecond

// Domain is one power domain register.
type Domain struct {
	Name string // device-tree node name, e.g. "power-controller@208"
	Addr uint64 // physical address of its register
}

// On brings the domain at r to ACTIVE. It writes only if the domain isn't
// already active, never sets the write-1-to-clear WAS_* bits, and releases
// RESET and DEV_DISABLE. switched reports whether it had to write.
func On(ctx context.Context, r mmio.Regs) (switched bool, err error) {
	v := r.Read32(0)
	if (v&PS_ACTUAL)>>4 == ACTIVE && v&(RESET|DEV_DISABLE) == 0 {
		return false, nil
	}
	v &^= WAS_CLKGATED | WAS_PWRGATED | PS_TARGET | RESET | DEV_DISABLE
	r.Write32(0, v|ACTIVE)
	deadline := time.Now().Add(Timeout)
	for {
		v = r.Read32(0)
		if (v&PS_ACTUAL)>>4 == ACTIVE {
			return true, nil
		}
		if time.Now().After(deadline) {
			return true, fmt.Errorf("still in state %#x after %v (register %#x)", (v&PS_ACTUAL)>>4, Timeout, v)
		}
		if err := ctx.Err(); err != nil {
			return true, err
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// FromFDT returns the domains a device needs, parents first, following
// "power-domains" up the tree. A domain's "reg" is an offset into its PMGR
// block (the parent node's first "reg" window). Each domain appears once.
func FromFDT(dev *fdt.Node) ([]Domain, error) {
	var out []Domain
	seen := map[*fdt.Node]bool{}
	var visit func(n *fdt.Node, depth int) error
	visit = func(n *fdt.Node, depth int) error {
		if depth > 32 {
			return errors.New("pmgr: power-domains loop")
		}
		refs, err := n.Refs("power-domains", "#power-domain-cells")
		if err != nil {
			return err
		}
		for _, ref := range refs {
			d := ref.Node
			if seen[d] {
				continue
			}
			seen[d] = true
			if err := visit(d, depth+1); err != nil { // parents first
				return err
			}
			addr, err := regAddr(d)
			if err != nil {
				return err
			}
			out = append(out, Domain{Name: d.Name, Addr: addr})
		}
		return nil
	}
	if err := visit(dev, 0); err != nil {
		return nil, err
	}
	return out, nil
}

func regAddr(d *fdt.Node) (uint64, error) {
	if !d.Compatible("apple,pmgr-pwrstate") {
		return 0, fmt.Errorf("pmgr: %s is not an apple,pmgr-pwrstate node", d.Path)
	}
	// The PMGR block has no "ranges", so Reg leaves the offset as it is
	// (the soc bus above it maps 1:1).
	reg, block := d.Reg(), d.Parent.Reg()
	if len(reg) == 0 || len(block) == 0 {
		return 0, fmt.Errorf("pmgr: %s: no reg, or its PMGR block has none", d.Path)
	}
	return block[0].Base + reg[0].Base, nil
}
