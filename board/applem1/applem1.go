//go:build tamago && arm64 && applem1

package applem1

import (
	"runtime/goos"
	_ "unsafe"

	"github.com/usbarmory/tamago/arm64"
)

// CPU is the boot core (one core: TamaGo has no arm64 SMP bring-up).
var CPU = &arm64.CPU{TimerOffset: 1}

// Init runs early in runtime setup (Hwinit1).
//
//go:linkname Init runtime/goos.Hwinit1
func Init() {
	CPU.Init()
	CPU.EnableCache()
	CPU.InitGenericTimers(0, 0) // 24 MHz, programmed by the firmware

	// CPU.Init installed TamaGo's vectors over the early ones: keep the
	// fault screen until the console exists (early.go).
	arm64.SystemExceptionHandler = lateFault
	stage(StageHwinit1)

	// Idle in WFE, woken by the timer event stream (idle.go).
	initIdle()

	// No PSCI: halt (and a fatal runtime error) stops here, with the
	// watchdog disarmed so the screen stays (hold the power button).
	// `reboot` resets through the watchdog (drivers.go).
	goos.Exit = func(int32) {
		stopWatchdog()
		for {
		}
	}
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return CPU.GetTime()
}
