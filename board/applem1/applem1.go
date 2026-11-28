//go:build tamago && arm64 && applem1

package applem1

import (
	"runtime/goos"
	_ "unsafe"

	"github.com/usbarmory/tamago/arm64"
)

// CPU is the boot core (the others: smp.go).
var CPU = &arm64.CPU{TimerOffset: 1}

// Init runs early in runtime setup (Hwinit1).
//
//go:linkname Init runtime/goos.Hwinit1
func Init() {
	blog("hwinit1: cpu")
	CPU.Init()
	blog(", caches")
	CPU.EnableCache()
	blog(", timers")
	CPU.InitGenericTimers(0, 0) // 24 MHz, programmed by the firmware
	blog("\n")

	// CPU.Init installed TamaGo's vectors over the early ones: keep the
	// fault screen until the console exists (early.go).
	arm64.SystemExceptionHandler = lateFault
	stage(StageHwinit1)

	// Idle in WFE, woken by the timer event stream (idle.go).
	initIdle()
	blog("idle: wfe\n")

	// No PSCI: halt (and a fatal runtime error) stops here, with the
	// watchdog disarmed so the screen stays (hold the power button).
	// `reboot` resets through the watchdog (drivers.go).
	// With the SMC up, halt powers the Mac off instead.
	goos.Exit = func(int32) {
		stopWatchdog()
		if s := smcDev.Load(); s != nil {
			s.PowerOffNow()
		}
		for {
		}
	}
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return CPU.GetTime()
}
