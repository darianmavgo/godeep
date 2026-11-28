//go:build tamago && arm64 && applem1

package applem1

import (
	"context"
	"errors"
	"sync"

	"godeep/kernel/dev"
	"godeep/kernel/fdt"
)

// Features the Mac leaves off at boot, turned on from the shell with
// `enable NAME` (programs/sh/enable.go): a boot that hangs or crash-loops
// can't be debugged, a shell can. Bootargs turn them on at boot instead:
// godeep.smp, godeep.agx.
//
//	smp  release the other 9 cores (smp.go)
//	agx  start the GPU driver (drivers/apple/agx) as gpu0

const featuresCompatible = "godeep,applem1-features"

var (
	bootRoot *fdt.Node // the device tree, for enable smp
	agxOff     *dev.Node // the GPU's node while it is off (devices.go)
	agxPresent bool      // the tree has a GPU node
)

func runFeatures(ctx context.Context, d *dev.Device) error {
	d.Ready(features{d})
	<-ctx.Done()
	return nil
}

type features struct{ d *dev.Device }

func (f features) Features() []dev.Feature {
	return []dev.Feature{smpFeature{}, agxFeature{f.d}}
}

type smpFeature struct{}

func (smpFeature) Name() string { return "smp" }
func (smpFeature) Help() string {
	return "release the other cores (GOMAXPROCS = cores); can't be undone without a reboot"
}
func (smpFeature) State() string { return SMPInfo() }
func (smpFeature) Enable(ctx context.Context) error {
	if bootRoot == nil {
		return errors.New("no device tree")
	}
	if smpStarted.Load() {
		return nil
	}
	blog("smp: start (enable smp)\n")
	startSMP(bootRoot)
	blog("smp: " + SMPInfo() + "\n")
	return nil
}

// agxFeature hands the GPU's node to the device manager, once.
type agxFeature struct{ d *dev.Device }

var agxMu sync.Mutex // guards agxOff, agxPresent

func (agxFeature) Name() string { return "agx" }
func (agxFeature) Help() string {
	return "start the GPU driver as gpu0 (stages 1-2: firmware boot, log; lsdev gpu0)"
}
func (agxFeature) State() string {
	agxMu.Lock()
	defer agxMu.Unlock()
	switch {
	case !agxPresent:
		return "no GPU in the device tree"
	case agxOff != nil:
		return "off"
	}
	return "on (lsdev gpu0)"
}

func (a agxFeature) Enable(ctx context.Context) error {
	agxMu.Lock()
	defer agxMu.Unlock()
	switch {
	case !agxPresent:
		return errors.New("no GPU in the device tree")
	case agxOff == nil:
		return nil // already on
	}
	a.d.Add(*agxOff)
	agxOff = nil
	return nil
}
