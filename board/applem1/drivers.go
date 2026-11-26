//go:build tamago && arm64 && applem1

package applem1

import (
	"godeep/drivers/simplefb"
	"sync/atomic"
	"time"

	"godeep/drivers/apple/ans"
	"godeep/drivers/apple/dwc3"
	"godeep/drivers/apple/fpwm"
	"godeep/drivers/apple/i2c"
	"godeep/drivers/apple/smc"
	"godeep/drivers/apple/spihid"
	"godeep/drivers/apple/spmi"
	"godeep/drivers/apple/wdt"
	"godeep/drivers/arm/pl011"
	"godeep/drivers/usb/xhci"
	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// WatchdogTimeout: if the kernel stops scheduling for this long, the
// watchdog resets the Mac.
const WatchdogTimeout = 30 * time.Second

// smcDev is set once the SMC answers: halt powers off through it.
var smcDev atomic.Pointer[smc.SMC]

// watchdog is set once the driver has armed it.
var watchdog atomic.Pointer[wdt.Watchdog]

func init() {
	window := func(base uint64) mmio.Regs { return mmio.Window{Base: base} }
	for _, d := range wdt.Drivers(window, WatchdogTimeout, func(w *wdt.Watchdog) { watchdog.Store(w) }) {
		dev.Register(d)
	}
	dev.Register(dev.Driver{Compatible: idleCompatible, Run: runIdleStats})
	dev.Register(spihid.Driver(window))
	dev.Register(simplefb.Driver())
	// USB-C ports (USB 2 host): port controllers on I2C, DWC3 + xHCI, and
	// what gets plugged in.
	dev.Register(i2c.Driver(window))
	dev.Register(dwc3.Driver(window))
	dev.Register(dwc3.GadgetDriver())
	dev.Register(xhci.DiskDriver())
	dev.Register(xhci.NICDriver())
	dev.Register(smc.Driver(func(base uint64) mmio.Regs64 { return mmio.Window{Base: base} }, func(s *smc.SMC) { smcDev.Store(s) }))
	dev.Register(smc.RTCDriver())
	dev.Register(spmi.Driver(window))
	dev.Register(fpwm.Driver(window))
	// The internal SSD (ANS2), read-only.
	dev.Register(ans.Driver(func(base uint64) mmio.Regs64 { return mmio.Window{Base: base} }))
	// QEMU only (tools/fakem1n1): typing in the terminal. The Mac has no PL011.
	dev.Register(pl011.Driver(window))
}

// stopWatchdog keeps the screen as it is: on halt, and on a fault or panic
// (so the message can be read and photographed instead of being wiped by a
// reset 30 s later).
func stopWatchdog() {
	if w := watchdog.Load(); w != nil {
		w.Stop()
	}
}
