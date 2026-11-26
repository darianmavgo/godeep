// Package wdt drives the Apple Silicon watchdog ("apple,wdt" and the
// per-SoC "apple,t8103-wdt", "apple,t6000-wdt"), after Linux's apple_wdt.c.
//
// The block has three timers; like Linux we use WD1. Each counts up at the
// 24 MHz reference clock: CUR_TIME is the count (write 0 to restart it), and
// when it reaches BITE_TIME with CTRL.RESET_EN set the SoC resets. The
// driver arms it at start and pets it from its own goroutine, so a kernel
// that stops scheduling (a spinning goroutine starving the rest, a hung
// idle loop) reboots instead of freezing. It is also the only way to reboot
// an Apple Silicon Mac without SMC: there is no PSCI.
package wdt

import (
	"context"
	"fmt"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// WD1 registers.
const (
	CUR_TIME  = 0x10
	BITE_TIME = 0x14
	CTRL      = 0x1c

	CTRL_IRQ_EN     = 1 << 0
	CTRL_IRQ_STATUS = 1 << 1
	CTRL_RESET_EN   = 1 << 2
)

// ClockHz is the watchdog's count rate (the SoC reference clock, "clkref"
// in the device tree; 24 MHz on every M1/M2 SoC).
const ClockHz = 24_000_000

// MaxTimeout is the longest timeout BITE_TIME can hold.
const MaxTimeout = time.Duration(1<<32-1) * time.Second / ClockHz

// Compatibles are the device-tree strings this driver handles.
var Compatibles = []string{"apple,t6000-wdt", "apple,t8103-wdt", "apple,wdt"}

// Watchdog is one armed WD1. Every method is a single 32-bit register write
// (or a short fixed sequence): safe to call from a fault path.
type Watchdog struct {
	r       mmio.Regs
	timeout time.Duration
}

// New returns a watchdog on regs. It doesn't touch the hardware.
func New(r mmio.Regs) *Watchdog { return &Watchdog{r: r} }

// Arm (re)starts the countdown with timeout. Stopped first, so a watchdog
// left running by the boot loader can't bite while BITE_TIME changes.
func (w *Watchdog) Arm(timeout time.Duration) {
	timeout = min(timeout, MaxTimeout)
	w.timeout = timeout
	w.r.Write32(CTRL, 0)
	w.r.Write32(CUR_TIME, 0)
	w.r.Write32(BITE_TIME, uint32(uint64(timeout)*ClockHz/uint64(time.Second)))
	w.r.Write32(CTRL, CTRL_RESET_EN)
}

// Pet restarts the countdown.
func (w *Watchdog) Pet() { w.r.Write32(CUR_TIME, 0) }

// Stop disarms the watchdog.
func (w *Watchdog) Stop() { w.r.Write32(CTRL, 0) }

// Reboot resets the machine now (Linux apple_wdt_restart): a zero timeout.
// The reset takes a few ms; Reboot returns, so callers should stop there.
func (w *Watchdog) Reboot() {
	w.r.Write32(CTRL, CTRL_RESET_EN)
	w.r.Write32(BITE_TIME, 0)
	w.r.Write32(CUR_TIME, 0)
	_ = w.r.Read32(CUR_TIME) // push the writes out
}

// Devices reports the watchdog's state for lsdev.
func (w *Watchdog) Devices() []string {
	if w.r.Read32(CTRL)&CTRL_RESET_EN == 0 {
		return []string{"stopped"}
	}
	left := time.Duration(0)
	bite, cur := w.r.Read32(BITE_TIME), w.r.Read32(CUR_TIME)
	if bite > cur {
		left = time.Duration(uint64(bite-cur) * uint64(time.Second) / ClockHz)
	}
	return []string{fmt.Sprintf("armed, %v timeout, bites in %v", w.timeout, left.Round(time.Millisecond))}
}

// Drivers returns a driver per compatible string. Each arms the watchdog
// with timeout, pets it every timeout/3, publishes the *Watchdog (a
// dev.Rebooter and dev.Lister), and stops it when the driver is stopped.
// armed, if not nil, gets the watchdog once it runs (a board keeps it to
// stop it on halt or on a fault screen).
func Drivers(regs func(base uint64) mmio.Regs, timeout time.Duration, armed func(*Watchdog)) []dev.Driver {
	run := func(ctx context.Context, d *dev.Device) error {
		if len(d.Reg) == 0 {
			return fmt.Errorf("wdt: no registers")
		}
		w := New(regs(d.Reg[0].Base))
		w.Arm(timeout)
		defer w.Stop()
		if armed != nil {
			armed(w)
		}
		d.Ready(w)
		t := time.NewTicker(w.timeout / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				w.Pet()
			}
		}
	}
	out := make([]dev.Driver, 0, len(Compatibles))
	for _, c := range Compatibles {
		out = append(out, dev.Driver{Compatible: c, Run: run})
	}
	return out
}
