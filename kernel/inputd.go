package kernel

import (
	"context"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/input"
	"godeep/kernel/proc"
)

// InputHandler is a console that responds to input events: a pointer, the
// wheel, selection (drivers/simplefb's Mouse).
type InputHandler interface{ HandleInput(input.Event) }

// How often inputd looks for new input devices: quickly while devices are
// coming up (boot, or after any change), then slowly, so an idle system
// isn't woken for it (a hot-plugged device takes up to inputScanIdle).
const (
	inputScanBusy  = 100 * time.Millisecond
	inputScanIdle  = time.Second
	inputBusyScans = 50 // quick scans without a change before slowing down
)

// startInput starts "inputd": it subscribes to every input source the
// device manager's drivers publish (USB mice and keyboards, the Mac's
// trackpad), including ones that appear later or come back after a driver
// restart, and hands every event to h, one at a time.
func startInput(ctx context.Context, tab *proc.Table, mgr *dev.Manager, h InputHandler) *proc.Proc {
	return tab.Spawn(ctx, 0, proc.Spec{Name: "inputd", Main: func(ctx context.Context, p *proc.Proc) error {
		events := make(chan input.Event, 256)
		subs := map[input.Source]context.CancelFunc{}
		t := time.NewTicker(inputScanBusy)
		defer t.Stop()
		quiet := 0 // scans since the last change
		scan := func() {
			changed := false
			live := map[input.Source]bool{}
			for _, s := range mgr.List() {
				if s.State != dev.Ready {
					continue
				}
				for _, src := range input.All(s.Service) {
					live[src] = true
					if subs[src] != nil {
						continue
					}
					changed = true
					sctx, cancel := context.WithCancel(ctx)
					subs[src] = cancel
					c := src.Subscribe(sctx)
					p.Go(func(context.Context) {
						for e := range c { // closed when sctx ends
							select {
							case events <- e:
							case <-sctx.Done():
							}
						}
					})
				}
			}
			// A source that's gone (its driver restarted or failed) is
			// unsubscribed; the restarted driver's new source is picked up
			// above.
			for src, cancel := range subs {
				if !live[src] {
					cancel()
					delete(subs, src)
					changed = true
				}
			}
			if changed {
				if quiet >= inputBusyScans {
					t.Reset(inputScanBusy)
				}
				quiet = 0
			} else if quiet++; quiet == inputBusyScans {
				t.Reset(inputScanIdle)
			}
		}
		scan()
		for {
			select {
			case <-ctx.Done():
				return nil
			case e := <-events:
				h.HandleInput(e)
			case <-t.C:
				scan()
			}
		}
	}})
}
