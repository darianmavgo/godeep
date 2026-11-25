// Package pl031 drives the ARM PrimeCell PL031 real-time clock.
package pl031

import (
	"context"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

const DR = 0x000 // data register: seconds since the epoch

type clock struct{ r mmio.Regs }

func (c clock) Now() time.Time { return time.Unix(int64(c.r.Read32(DR)), 0) }

// Driver returns the "arm,pl031" driver, which publishes a dev.Clock.
func Driver(regs func(base uint64) mmio.Regs) dev.Driver {
	return dev.Driver{Compatible: "arm,pl031", Run: func(ctx context.Context, d *dev.Device) error {
		d.Ready(dev.Clock(clock{regs(d.Reg[0].Base)}))
		<-ctx.Done()
		return nil
	}}
}
