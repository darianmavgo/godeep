package sh

import (
	"context"
	"errors"
	"fmt"

	"image/color"
	"strconv"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/gpu"
	"godeep/kernel/input"
	"godeep/kernel/proc"
	"godeep/kernel/wm"
)

func init() {
	builtins["wm"] = builtin{"wm [SECONDS]: a desktop on the first display: windows (a clock, an input log, a test pattern) you can click and drag; ^C or kill ends it", wmCmd}
}

// wmCmd runs the compositor (kernel/wm) on the first gpu.Display, feeds it
// every input device, and opens three demo windows. Everything belongs to
// this process: when it ends, the display goes blank.
func wmCmd(ctx context.Context, env Env, p *proc.Proc, args []string) error {
	var until <-chan time.Time
	if len(args) == 1 {
		secs, err := strconv.ParseFloat(args[0], 64)
		if err != nil || secs <= 0 {
			return fmt.Errorf("bad SECONDS %q", args[0])
		}
		until = time.After(time.Duration(secs * float64(time.Second)))
	} else if len(args) > 1 {
		return errors.New("usage: wm [SECONDS]")
	}
	if env.Dev == nil {
		return errors.New("no devices")
	}
	var d gpu.Display
	var name string
	for _, s := range env.Dev.List() {
		if g, ok := s.Service.(gpu.Display); ok && s.State == dev.Ready {
			d, name = g, s.Name
			break
		}
	}
	if d == nil {
		return errors.New("no display (QEMU: -device virtio-gpu-device)")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := wm.New(d)
	ran := make(chan error, 1)
	if !p.Go(func(context.Context) { ran <- m.Run(ctx) }) {
		return proc.ErrKilled
	}
	p.Go(func(context.Context) { wmInput(ctx, env.Dev, m, p) })

	clock, err := m.NewWindow(ctx, "clock", 240, 40)
	if err != nil {
		return err
	}
	p.Go(func(context.Context) {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			gpu.Fill(clock, clock.Bounds(), color.RGBA{0xf4, 0xf4, 0xf0, 0xff})
			wm.DrawText(clock, 10, 14, time.Now().Format("15:04:05  Mon Jan 2 2006"), color.RGBA{0x20, 0x20, 0x20, 0xff})
			if clock.Present(ctx, clock.Bounds()) != nil {
				return
			}
			select {
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
	})
	log, err := m.NewWindow(ctx, "input", 320, 140)
	if err != nil {
		return err
	}
	p.Go(func(context.Context) {
		var lines []string
		for {
			gpu.Fill(log, log.Bounds(), color.RGBA{0x18, 0x18, 0x18, 0xff})
			for i, l := range lines {
				wm.DrawText(log, 6, 4+14*i, l, color.RGBA{0x9c, 0xff, 0x9c, 0xff})
			}
			if log.Present(ctx, log.Bounds()) != nil {
				return
			}
			select {
			case e := <-log.Events():
				lines = append(lines, e.String())
				if len(lines) > 9 {
					lines = lines[1:]
				}
			case <-ctx.Done():
				return
			}
		}
	})
	pattern, err := m.NewWindow(ctx, "pattern", 320, 200)
	if err != nil {
		return err
	}
	gpu.TestPattern(pattern)
	if err := pattern.Present(ctx, pattern.Bounds()); err != nil {
		return err
	}
	at, _ := pattern.Position()
	sz := d.Size()
	fmt.Fprintf(env.out(), "wm: %s %dx%d, 3 windows, pattern at %d,%d\n", name, sz.X, sz.Y, at.X, at.Y)

	var intr <-chan struct{}
	if env.TTY != nil && env.Out == nil {
		intr = env.TTY.Interrupts()
	}
	select {
	case <-until:
	case <-intr:
	case <-ctx.Done():
	case err := <-ran:
		return err
	}
	return nil
}

// wmInput subscribes the compositor to every input source, like inputd
// (rescanning for devices that appear later).
func wmInput(ctx context.Context, mgr *dev.Manager, m *wm.Manager, p *proc.Proc) {
	subs := map[input.Source]bool{}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		for _, s := range mgr.List() {
			if s.State != dev.Ready {
				continue
			}
			for _, src := range input.All(s.Service) {
				if subs[src] {
					continue
				}
				subs[src] = true
				c := src.Subscribe(ctx)
				p.Go(func(context.Context) {
					for e := range c {
						m.Input(e)
					}
				})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
