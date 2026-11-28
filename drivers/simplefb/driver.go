package simplefb

import (
	"context"
	"errors"
	"image"

	"godeep/kernel/dev"
	"godeep/kernel/gpu"
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

// status is the fbcon device's service: lsdev's view, and the framebuffer
// as a gpu.Display (display.go) for the compositor.
type status struct{ c *Console }

func (s status) Devices() []string { return []string{s.c.Status()} }

func (s status) Size() image.Point { return s.c.Display().Size() }
func (s status) NewSurface(ctx context.Context, w, h int) (gpu.Surface, error) {
	return s.c.Display().NewSurface(ctx, w, h)
}
func (s status) Show(ctx context.Context, sf gpu.Surface) error { return s.c.Display().Show(ctx, sf) }

// ConsoleDisplay marks the console's framebuffer: programs prefer another
// display (virtio-gpu) when there is one.
func (status) ConsoleDisplay() {}
