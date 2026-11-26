//go:build tamago && arm64 && applem1

package applem1

import (
	"time"

	"errors"
	"fmt"
	"unsafe"

	"godeep/drivers/apple/aic"
	"godeep/drivers/apple/ans"
	"godeep/drivers/apple/dwc3"
	"godeep/drivers/apple/fpwm"
	"godeep/drivers/apple/i2c"
	"godeep/drivers/apple/pmgr"
	"godeep/drivers/apple/smc"
	"godeep/drivers/apple/spihid"
	"godeep/drivers/apple/spmi"
	"godeep/drivers/simplefb"
	"godeep/drivers/usb/xhci"
	"godeep/kernel/dev"
	"godeep/kernel/fdt"
	"godeep/kernel/irq"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
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

// configureNodes configures the SSD (named nvme0), the USB-C controllers (dwc3) and the I2C buses their
// port controllers are on, in place in nodes. Keyboards type into keys.
func configureNodes(root *fdt.Node, nodes []dev.Node, keys chan<- byte) (extra []dev.Node) {
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
		case ans.Compatible:
			n.Name = "nvme0" // like QEMU's NVMe disk: nvme0p1, nvme0p2 ...
			if c, err := ans.ConfigFromFDT(fn); err != nil {
				n.Config = err
			} else {
				c.Writes = ssdWrites
				n.Config = c
			}
		case smc.Compatible:
			n.Name = "smc"
			n.Config = smcConfig(fn)
			if rc, err := rtcConfig(fn); err == nil {
				extra = append(extra, dev.Node{Name: "rtc", Compatible: smc.RTCCompatible, Config: rc,
					Needs: []string{"smc", rc.SPMI}})
			}
		case fpwm.Compatible:
			n.Name = "kbdlight"
			n.Config = fpwmConfig(fn)
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
	return extra
}

// bootEntropy mixes /chosen's rng-seed and kaslr-seed into the DRBG and
// wipes them from the tree, and says how many bytes it found.
func bootEntropy(root *fdt.Node) string {
	ch := root.Find("/chosen")
	n := 0
	if ch != nil {
		for _, p := range []string{"rng-seed", "kaslr-seed"} {
			if b := ch.Props[p]; len(b) > 0 {
				mixSeed(b)
				n += len(b)
				clear(b)
			}
		}
	}
	if n < 32 {
		return fmt.Sprintf("WARNING: only %d bytes of boot entropy in /chosen: crypto/rand is predictable", n)
	}
	return fmt.Sprintf("entropy: %d bytes from /chosen seeds", n)
}

// smcConfig resolves the SMC node: "smc" (ASC CPU control), "sram", and
// its mailbox.
func smcConfig(n *fdt.Node) any {
	var c smc.Config
	reg := n.Reg()
	for i, name := range n.Strings("reg-names") {
		if i < len(reg) {
			switch name {
			case "smc":
				c.CPU = reg[i].Base
			case "sram":
				c.SRAM, c.SRAMSize = reg[i].Base, reg[i].Size
			}
		}
	}
	mb, err := n.Refs("mboxes", "#mbox-cells")
	if err != nil || len(mb) == 0 || len(mb[0].Node.Reg()) == 0 || c.CPU == 0 || c.SRAM == 0 {
		return fmt.Errorf("smc: incomplete node %s (%v)", n.Path, err)
	}
	c.Mailbox = mb[0].Node.Reg()[0].Base
	c.MailboxIRQs, _ = aic.IRQs(mb[0].Node)
	return c
}

// interrupts finds the AIC2, masks every line and enables it, and starts
// the "irq" kernel thread that reads its event register every IRQPoll and
// dispatches to the bridge. nil if the tree has no AIC.
func interrupts(root *fdt.Node) (*irq.Bridge, string) {
	var n *fdt.Node
	root.Walk(func(x *fdt.Node) {
		if n == nil && x.Compatible(aic.Compatible) {
			n = x
		}
	})
	if n == nil {
		return nil, "no AIC in the device tree: drivers poll"
	}
	reg := n.Reg()
	var core, event uint64
	for i, name := range n.Strings("reg-names") {
		if i < len(reg) {
			switch name {
			case "core":
				core = reg[i].Base
			case "event":
				event = reg[i].Base
			}
		}
	}
	if core == 0 || event == 0 {
		return nil, "AIC node lacks core/event registers: drivers poll"
	}
	a, err := aic.New(mmio.Window{Base: core}, mmio.Window{Base: event})
	if err != nil {
		return nil, err.Error() + ": drivers poll"
	}
	a.Init()
	b := irq.NewBridge(a)
	proc.Kthread("irq", func() {
		for {
			for {
				kind, id := a.Event()
				if kind == 0 {
					break
				}
				if kind == aic.EventIRQ {
					b.Dispatch(id)
				}
			}
			time.Sleep(IRQPoll)
		}
	})
	return b, fmt.Sprintf("AIC2: %d IRQs, %d dies, events polled every %v", a.NrIRQ, a.NrDie, IRQPoll)
}

// IRQPoll is how often the irq thread reads the AIC's event register.
const IRQPoll = time.Millisecond

// rtcConfig follows the SMC's rtc child to the PMIC cell holding the RTC
// offset: nvmem-cells -> rtc-offset@1411 (reg: address, size) inside
// pmic@f (reg: slave ID) on the SPMI bus.
func rtcConfig(smcNode *fdt.Node) (smc.RTCConfig, error) {
	var c smc.RTCConfig
	var rtc *fdt.Node
	for _, ch := range smcNode.Children {
		if ch.Name == "rtc" {
			rtc = ch
		}
	}
	if rtc == nil {
		return c, errors.New("rtc: no rtc under the SMC")
	}
	cells := rtc.U32s("nvmem-cells")
	if len(cells) == 0 {
		return c, errors.New("rtc: no nvmem-cells")
	}
	cell := rtc.ByPhandle(cells[0])
	if cell == nil || len(cell.U32s("reg")) < 2 {
		return c, errors.New("rtc: bad rtc-offset cell")
	}
	var pmic *fdt.Node
	for p := cell.Parent; p != nil; p = p.Parent {
		if p.Parent != nil && p.Parent.Compatible(spmi.Compatible) {
			pmic = p
			break
		}
	}
	if pmic == nil || len(pmic.U32s("reg")) == 0 {
		return c, errors.New("rtc: the cell is not on an SPMI PMIC")
	}
	c.SPMI, c.SID, c.Addr = pmic.Parent.Name, byte(pmic.U32s("reg")[0]), uint16(cell.U32s("reg")[0])
	return c, nil
}

// fpwmConfig resolves the backlight PWM: its clock, power domains, and the
// period the pwm-leds node asks for.
func fpwmConfig(n *fdt.Node) any {
	c := fpwm.Config{ClockHz: 24_000_000, Period: 40 * time.Microsecond}
	if clk, err := n.Refs("clocks", "#clock-cells"); err == nil && len(clk) > 0 {
		if f := clk[0].Node.U32s("clock-frequency"); len(f) == 1 {
			c.ClockHz = uint64(f[0])
		}
	}
	ph := n.Phandle()
	n.Root().Walk(func(x *fdt.Node) {
		if p := x.U32s("pwms"); len(p) >= 3 && p[0] == ph && p[2] > 0 {
			c.Period = time.Duration(p[2])
		}
	})
	doms, err := pmgr.FromFDT(n)
	if err != nil {
		return err
	}
	c.Power = doms
	return c
}

// ssdWrites lets godeep write to the internal SSD, fenced to the FAT32
// partition labelled GODEEPDATA (block.Fence). Turn it on only after a
// boot has shown `lsblk` and `ls nvme0p…:/` reading the SSD correctly.
const ssdWrites = false
