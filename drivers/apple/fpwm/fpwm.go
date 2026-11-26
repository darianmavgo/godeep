// Package fpwm drives the M1 Pro's keyboard backlight: Apple's FPWM
// ("apple,t6000-fpwm"), after Linux's pwm-apple.c. The LED's period comes
// from the tree (pwm-leds: 40 µs); brightness is the on-time share.
package fpwm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"godeep/drivers/apple/pmgr"
	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// Compatible is the controller in the M1 Pro's device tree.
const Compatible = "apple,t6000-fpwm"

const (
	CTRL       = 0x00
	OFF_CYCLES = 0x18
	ON_CYCLES  = 0x1c

	CTRL_ENABLE        = 1 << 0
	CTRL_UPDATE        = 1 << 5
	CTRL_OUTPUT_ENABLE = 1 << 14
)

// Config is what the board resolves.
type Config struct {
	ClockHz uint64
	Period  time.Duration // from the LED's "pwms"
	Power   []pmgr.Domain
}

// Backlight is the driver's service.
type Backlight struct {
	mu      sync.Mutex
	r       mmio.Regs
	cycles  uint64 // per period
	percent int
}

// Set turns the backlight to percent (0 off .. 100 full).
func (b *Backlight) Set(percent int) {
	percent = max(0, min(percent, 100))
	b.mu.Lock()
	defer b.mu.Unlock()
	b.percent = percent
	if percent == 0 {
		b.r.Write32(CTRL, 0)
		return
	}
	on := b.cycles * uint64(percent) / 100
	b.r.Write32(ON_CYCLES, uint32(on))
	b.r.Write32(OFF_CYCLES, uint32(b.cycles-on))
	b.r.Write32(CTRL, CTRL_ENABLE|CTRL_OUTPUT_ENABLE|CTRL_UPDATE)
}

// Get returns the brightness last set, or read back from the hardware.
func (b *Backlight) Get() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.percent
}

func (b *Backlight) Devices() []string {
	return []string{fmt.Sprintf("keyboard backlight %d%% (kbdlight N to change)", b.Get())}
}

// New reads the current state (iBoot may have left it on).
func New(r mmio.Regs, clockHz uint64, period time.Duration) *Backlight {
	b := &Backlight{r: r, cycles: clockHz * uint64(period) / uint64(time.Second)}
	if b.cycles == 0 {
		b.cycles = 960 // 40 µs at 24 MHz
	}
	if c := r.Read32(CTRL); c&CTRL_ENABLE != 0 && c&CTRL_OUTPUT_ENABLE != 0 {
		on, off := uint64(r.Read32(ON_CYCLES)), uint64(r.Read32(OFF_CYCLES))
		if on+off > 0 {
			b.percent = int(100 * on / (on + off))
		}
	}
	return b
}

// Driver publishes the backlight.
func Driver(regs func(base uint64) mmio.Regs) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		c, ok := d.Config.(Config)
		if !ok || len(d.Reg) == 0 {
			return errors.New("fpwm: no configuration from the board")
		}
		for _, p := range c.Power {
			if _, err := pmgr.On(ctx, regs(p.Addr)); err != nil {
				return fmt.Errorf("fpwm: power domain %s: %w", p.Name, err)
			}
		}
		d.Ready(New(regs(d.Reg[0].Base), c.ClockHz, c.Period))
		<-ctx.Done()
		return nil
	}}
}
