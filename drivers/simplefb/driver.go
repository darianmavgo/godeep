package simplefb

import (
	"context"
	"errors"

	"godeep/kernel/dev"
)

// DeviceCompatible is the device-manager entry boards add for their
// framebuffer console (Config: the *Console), so lsdev shows its state:
// size, history, scroll position, pointer, selection.
const DeviceCompatible = "godeep,fbcon"

// Driver returns the fbcon status driver.
func Driver() dev.Driver {
	return dev.Driver{Compatible: DeviceCompatible, Run: func(ctx context.Context, d *dev.Device) error {
		c, ok := d.Config.(*Console)
		if !ok {
			return errors.New("fbcon: no console")
		}
		d.Ready(status{c})
		<-ctx.Done()
		return nil
	}}
}

type status struct{ c *Console }

func (s status) Devices() []string { return []string{s.c.Status()} }
