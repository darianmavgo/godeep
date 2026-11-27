package cap

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type fakeNet struct{}

func (fakeNet) Listen(context.Context, string, string) (net.Listener, error) { return nil, nil }
func (fakeNet) Dial(context.Context, string, string) (net.Conn, error)       { return nil, nil }

func TestPendingNet(t *testing.T) {
	p := NewPendingNet()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Listen(ctx, "tcp", ":22"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Listen before Resolve: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := p.Dial(context.Background(), "tcp", "1.2.3.4:5"); done <- err }()
	p.Resolve(fakeNet{}, nil)
	p.Resolve(nil, errors.New("ignored: only the first Resolve counts"))
	if err := <-done; err != nil {
		t.Fatalf("Dial after Resolve: %v", err)
	}

	q := NewPendingNet()
	q.Resolve(nil, nil)
	if _, err := q.Wait(context.Background()); !errors.Is(err, ErrNoNetwork) {
		t.Fatalf("no network: %v", err)
	}
}

func TestRestricted(t *testing.T) {
	ctx := context.Background()
	p := NewPendingNet()
	p.Resolve(fakeNet{}, nil)
	d := DialOnly(p)
	if _, err := d.Listen(ctx, "tcp", ":80"); !errors.Is(err, ErrPermission) {
		t.Fatalf("Listen: %v", err)
	}
	if _, err := d.Dial(ctx, "tcp", "1.2.3.4:5"); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	none := &Restricted{N: d, NoDial: true}
	if _, err := none.Dial(ctx, "tcp", "1.2.3.4:5"); !errors.Is(err, ErrPermission) {
		t.Fatalf("Dial on no-dial: %v", err)
	}
	// Narrowing again can't widen: the inner DialOnly still refuses Listen.
	wide := &Restricted{N: d}
	if _, err := wide.Listen(ctx, "tcp", ":80"); !errors.Is(err, ErrPermission) {
		t.Fatalf("re-wrapped Listen: %v", err)
	}
	if u, err := Underlying(ctx, none); err != nil || u != (fakeNet{}) {
		t.Fatalf("Underlying = %v, %v", u, err)
	}
	if DialOnly(nil) != nil {
		t.Fatal("DialOnly(nil) should stay nil")
	}
}
