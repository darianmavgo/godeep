package kernel

import (
	"context"
	"sync"
	"testing"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/input"
	"godeep/kernel/proc"
)

type recorder struct {
	mu sync.Mutex
	ev []input.Event
}

func (r *recorder) HandleInput(e input.Event) { r.mu.Lock(); r.ev = append(r.ev, e); r.mu.Unlock() }
func (r *recorder) count() int                { r.mu.Lock(); defer r.mu.Unlock(); return len(r.ev) }

type twoSources struct{ mouse, kbd *input.Broadcaster }

func (t twoSources) InputSources() []input.Source { return []input.Source{t.mouse, t.kbd} }

// sendUntil keeps sending (subscriptions start asynchronously) until the
// recorder has n events.
func sendUntil(t *testing.T, r *recorder, n int, b *input.Broadcaster, e input.Event) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("events never arrived (have %d, want %d)", r.count(), n)
		}
		b.Send(e)
		time.Sleep(20 * time.Millisecond)
	}
}

func TestInputdFollowsDevices(t *testing.T) {
	srcs := make(chan twoSources, 4)
	crash := make(chan struct{}, 1)
	dev.Register(dev.Driver{Compatible: "test,hid", Run: func(ctx context.Context, d *dev.Device) error {
		s := twoSources{&input.Broadcaster{Kind: "mouse"}, &input.Broadcaster{Kind: "keyboard"}}
		d.Ready(s)
		srcs <- s
		select {
		case <-ctx.Done():
			return nil
		case <-crash:
			return context.Canceled // anything non-nil: devmgr restarts it
		}
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tab := proc.NewTable()
	mgr := dev.NewManager(tab, nil, nil)
	mgr.Start(ctx, []dev.Node{{Name: "hid0", Compatible: "test,hid"}})
	r := &recorder{}
	inputd := startInput(ctx, tab, mgr, r)

	first := <-srcs
	sendUntil(t, r, 1, first.mouse, input.Event{Kind: input.Motion, DX: 1})
	sendUntil(t, r, r.count()+1, first.kbd, input.Event{Kind: input.KeyDown, Code: 4})

	crash <- struct{}{}
	second := <-srcs
	sendUntil(t, r, r.count()+1, second.mouse, input.Event{Kind: input.Wheel, DY: 1})
	waitUnsubscribed := time.Now().Add(5 * time.Second)
	for {
		if _, _, subs := first.mouse.Stats(); subs == 0 {
			break
		}
		if time.Now().After(waitUnsubscribed) {
			t.Fatal("still subscribed to the crashed driver's source")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-inputd.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("inputd ignored kill")
	}
}
