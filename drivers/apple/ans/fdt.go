package ans

import (
	"fmt"

	"godeep/drivers/apple/aic"
	"godeep/drivers/apple/pmgr"
	"godeep/kernel/fdt"
)

// ConfigFromFDT resolves an "apple,t6000-nvme-ans2" node: its "nvme" and
// "ans" windows, its mailbox ("mboxes"), SART ("apple,sart"), power
// domains and reset domain ("resets").
func ConfigFromFDT(n *fdt.Node) (Config, error) {
	var c Config
	reg := n.Reg()
	for i, name := range n.Strings("reg-names") {
		if i >= len(reg) {
			break
		}
		switch name {
		case "nvme":
			c.NVMe = reg[i].Base
		case "ans":
			c.ANS = reg[i].Base
		}
	}
	if c.NVMe == 0 || c.ANS == 0 {
		return c, fmt.Errorf("ans: %s lacks nvme/ans registers", n.Path)
	}
	mb, err := n.Refs("mboxes", "#mbox-cells")
	if err != nil || len(mb) == 0 || len(mb[0].Node.Reg()) == 0 {
		return c, fmt.Errorf("ans: %s: no mailbox (%v)", n.Path, err)
	}
	c.Mailbox = mb[0].Node.Reg()[0].Base
	c.MailboxIRQs, _ = aic.IRQs(mb[0].Node)
	sa, err := n.Refs("apple,sart", "#sart-cells")
	if err != nil || len(sa) == 0 || len(sa[0].Node.Reg()) == 0 {
		return c, fmt.Errorf("ans: %s: no SART (%v)", n.Path, err)
	}
	c.SART = sa[0].Node.Reg()[0].Base
	seen := map[uint64]bool{}
	for _, x := range []*fdt.Node{mb[0].Node, sa[0].Node, n} {
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
	if rs, err := n.Refs("resets", "#reset-cells"); err == nil && len(rs) > 0 {
		doms, err := pmgr.FromFDT(n)
		if err == nil {
			for _, d := range doms {
				if d.Name == rs[0].Node.Name {
					d := d
					c.Reset = &d
				}
			}
		}
	}
	return c, nil
}
