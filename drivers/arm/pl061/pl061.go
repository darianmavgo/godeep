// Package pl061 drives the ARM PrimeCell PL061 GPIO controller as a power
// button (QEMU virt wires its power button to line 3). Interrupt-driven.
package pl061

import (
	"context"

	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// Registers.
const (
	IS  = 0x404 // interrupt sense (0 = edge)
	IBE = 0x408 // both edges
	IEV = 0x40c // event (1 = rising)
	IE  = 0x410 // interrupt enable
	MIS = 0x418 // masked interrupt status
	IC  = 0x41c // interrupt clear (write-1-to-clear)
)

// Driver returns the "arm,pl061" driver. regs maps a register base to its
// window; onPress runs (in the driver goroutine) when line `button` rises.
func Driver(regs func(base uint64) mmio.Regs, button uint, onPress func()) dev.Driver {
	bit := uint32(1) << button
	return dev.Driver{Compatible: "arm,pl061", Run: func(ctx context.Context, d *dev.Device) error {
		r := regs(d.Reg[0].Base)
		line, err := d.Bridge.Request(d.IRQ0(), d.Name)
		if err != nil {
			return err
		}
		defer line.Release()
		mmio.Modify(r, IS, bit, 0)  // edge-triggered
		mmio.Modify(r, IBE, bit, 0) // single edge
		mmio.Modify(r, IEV, 0, bit) // rising
		r.Write32(IC, bit)          // clear stale events (W1C: only our bit)
		mmio.Modify(r, IE, 0, bit)  // enable
		d.Ready("power button")
		for {
			select {
			case <-line.C:
				mis := r.Read32(MIS)
				r.Write32(IC, mis)
				line.Done()
				if mis&bit != 0 {
					onPress()
				}
			case <-ctx.Done():
				return nil
			}
		}
	}}
}
