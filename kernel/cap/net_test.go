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
