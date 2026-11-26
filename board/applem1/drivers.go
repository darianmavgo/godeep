//go:build tamago && arm64 && applem1

package applem1

import (
	"godeep/drivers/simplefb"
	"sync/atomic"
	"time"

	"godeep/drivers/apple/spihid"
	"godeep/drivers/apple/wdt"
	"godeep/drivers/arm/pl011"
	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// WatchdogTimeout: if the kernel stops scheduling for this long, the
// watchdog resets the Mac.
const WatchdogTimeout = 30 * time.Second

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
