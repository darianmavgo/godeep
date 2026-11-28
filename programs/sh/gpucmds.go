package sh

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"strconv"
	"time"

	"godeep/kernel/gpu"
	"godeep/kernel/proc"
)

func init() {
	builtins["gpu"] = builtin{"gpu [test [SECONDS] | fill RRGGBB | off]: displays; paint a test pattern or a color (kept until SECONDS pass, or this shell exits)", gpuCmd}
}

// gpuCmd draws on the first gpu.Display the device manager has. The
// surface belongs to the shell (its ctx): it stays on screen until the
// shell exits, or SECONDS pass.
func gpuCmd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) == 0 { // list them
		for _, s := range env.Dev.List() {
			if g, ok := s.Service.(gpu.Display); ok {
				sz := g.Size()
				fmt.Fprintf(env.out(), "%s: %dx%d\n", s.Name, sz.X, sz.Y)
			}
		}
		return nil
	}
	d, name, err := findDisplay(env) // a GPU before the console's framebuffer
	if err != nil {
		return err
	}
	sctx := ctx
	var c *color.RGBA
	switch {
	case args[0] == "off" && len(args) == 1:
		return d.Show(ctx, nil)
	case args[0] == "test" && len(args) <= 2:
		if len(args) == 2 {
			secs, err := strconv.ParseFloat(args[1], 64)
			if err != nil || secs <= 0 {
				return fmt.Errorf("bad SECONDS %q", args[1])
			}
			var cancel context.CancelFunc
			sctx, cancel = context.WithTimeout(ctx, time.Duration(secs*float64(time.Second)))
			defer cancel()
		}
	case args[0] == "fill" && len(args) == 2:
		v, err := strconv.ParseUint(args[1], 16, 32)
		if err != nil || len(args[1]) != 6 {
			return fmt.Errorf("bad color %q (RRGGBB)", args[1])
		}
		c = &color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
	default:
		return errors.New("usage: gpu [test [SECONDS] | fill RRGGBB | off]")
	}
	sz := d.Size()
	s, err := d.NewSurface(sctx, sz.X, sz.Y)
	if err != nil {
		return err
	}
	if c != nil {
		gpu.Fill(s, s.Bounds(), *c)
	} else {
		gpu.TestPattern(s)
	}
	if err := d.Show(ctx, s); err != nil {
		return err
	}
	fmt.Fprintf(env.out(), "%s: showing %dx%d\n", name, sz.X, sz.Y)
	if sctx != ctx { // SECONDS or ^C: then the surface goes away
		var intr <-chan struct{}
		if env.TTY != nil && env.Out == nil {
			intr = env.TTY.Interrupts()
		}
		select {
		case <-sctx.Done():
		case <-intr:
		}
	}
	return nil
}
