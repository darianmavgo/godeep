// Package mmio probes one virtio-mmio transport slot and reports which
// virtio device (if any) is attached. Device drivers (rng, blk, net) will
// build on this.
package mmio

import (
	"context"
	"fmt"

	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

const (
	MagicValue = 0x000 // "virt"
	Version    = 0x004
	DeviceID   = 0x008
	VendorID   = 0x00c

	Magic = 0x74726976
)

var names = map[uint32]string{1: "net", 2: "blk", 3: "console", 4: "rng", 9: "9p", 16: "gpu", 18: "input"}

// Present reports whether a device sits behind the window (empty slots read
// DeviceID 0).
func Present(r mmio.Regs) bool { return r.Read32(MagicValue) == Magic && r.Read32(DeviceID) != 0 }

// Describe names the attached device, e.g. "rng (virtio 1.2? v2)".
func Describe(r mmio.Regs) string {
	if r.Read32(MagicValue) != Magic {
		return "not a virtio-mmio device"
	}
	id := r.Read32(DeviceID)
	if id == 0 {
		return "empty slot"
	}
	n := names[id]
	if n == "" {
		n = fmt.Sprintf("device id %d", id)
	}
	return fmt.Sprintf("%s (transport v%d, vendor %#x)", n, r.Read32(Version), r.Read32(VendorID))
}

type desc []string

func (d desc) Devices() []string { return d }

// Driver returns a probe-only "virtio,mmio" driver: it publishes what's in
// the slot and exits.
func Driver(regs func(base uint64) mmio.Regs) dev.Driver {
	return dev.Driver{Compatible: "virtio,mmio", Run: func(ctx context.Context, d *dev.Device) error {
		d.Ready(dev.Lister(desc{Describe(regs(d.Reg[0].Base))}))
		return nil
	}}
}
