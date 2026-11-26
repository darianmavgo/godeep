package spihid

import (
	"errors"
	"fmt"
	"time"

	"godeep/drivers/apple/gpio"
	"godeep/drivers/apple/pmgr"
	"godeep/kernel/fdt"
)

// Defaults when the tree doesn't say (Asahi's t600x values).
const (
	defaultClockHz = 24_000_000
	defaultSpeedHz = 8_000_000
)

// Interrupt flags (dt-bindings/interrupt-controller/irq.h).
const (
	irqEdgeRising = 1
	irqLevelHigh  = 4
)

// ConfigFromFDT resolves a "apple,spi-hid-transport" node (a child of its
// SPI controller) into a Config. Keys is left for the board to set.
func ConfigFromFDT(t *fdt.Node) (Config, error) {
	var c Config
	ctl := t.Parent
	if ctl == nil || !ctl.Compatible("apple,spi") {
		return c, fmt.Errorf("spihid: %s is not under an apple,spi controller", t.Path)
	}
	if st := ctl.Strings("status"); len(st) > 0 && st[0] != "okay" && st[0] != "ok" {
		return c, fmt.Errorf("spihid: controller %s is %q", ctl.Path, st[0])
	}
	reg := ctl.Reg()
	if len(reg) == 0 {
		return c, fmt.Errorf("spihid: %s has no registers", ctl.Path)
	}
	c.SPI = reg[0].Base

	c.ClockHz = defaultClockHz
	if clk, err := ctl.Refs("clocks", "#clock-cells"); err == nil && len(clk) > 0 {
		if f := clk[0].Node.U32s("clock-frequency"); len(f) == 1 {
			c.ClockHz = uint64(f[0])
		}
	}
	c.SpeedHz = defaultSpeedHz
	if f := t.U32s("spi-max-frequency"); len(f) == 1 && f[0] > 0 {
		c.SpeedHz = uint64(f[0])
	}
	ns := func(prop string) time.Duration {
		if v := t.U32s(prop); len(v) == 1 {
			return time.Duration(v[0])
		}
		return 0
	}
	c.CSSetup, c.CSHold, c.CSInactive = ns("spi-cs-setup-delay-ns"), ns("spi-cs-hold-delay-ns"), ns("spi-cs-inactive-delay-ns")

	en, err := t.Refs("spien-gpios", "#gpio-cells")
	if err != nil {
		return c, err
	}
	if len(en) == 0 {
		return c, errors.New("spihid: no spien-gpios")
	}
	if c.Enable, err = gpio.PinFromRef(en[0]); err != nil {
		return c, err
	}
	blocks := []*fdt.Node{en[0].Node}

	// The interrupt line, polled. Not an error if it isn't a GPIO: the
	// driver then reads blind.
	if irq, err := t.Refs("interrupts-extended", "#interrupt-cells"); err == nil && len(irq) > 0 {
		if p, err := gpio.PinFromRef(irq[0]); err == nil {
			c.Ready = &p
			c.ReadyActiveHigh = p.Flags&(irqEdgeRising|irqLevelHigh) != 0
			blocks = append(blocks, irq[0].Node)
		}
	}

	if c.Mux, err = gpio.MuxFromFDT(ctl); err != nil {
		return c, err
	}

	// Power: the GPIO blocks first (spien is driven before any transfer),
	// then the controller. Each chain is parents first; shared parents once.
	seen := map[uint64]bool{}
	for _, n := range append(blocks, ctl) {
		doms, err := pmgr.FromFDT(n)
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
	return c, nil
}
