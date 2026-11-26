package kernel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/netdev"
	"godeep/kernel/proc"
)

// testNIC is a NIC that stops (ErrDown) when its driver instance does.
type testNIC struct {
	gen  int
	rx   chan []byte
	sent chan []byte
	down chan struct{}
}

func (n *testNIC) MAC() [6]byte { return [6]byte{2, 0, 0, 0, 0, byte(n.gen)} }
func (n *testNIC) MTU() int     { return 1500 }
func (n *testNIC) Send(ctx context.Context, f []byte) error {
	select {
	case n.sent <- f:
		return nil
	case <-n.down:
		return netdev.ErrDown
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (n *testNIC) Recv(ctx context.Context) ([]byte, error) {
	select {
	case f := <-n.rx:
		return f, nil
	case <-n.down:
		return nil, netdev.ErrDown
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestFollowNICAcrossRestart(t *testing.T) {
	nics := make(chan *testNIC, 4)
	crash := make(chan struct{}, 1)
	var gen atomic.Int32
	dev.Register(dev.Driver{Compatible: "test,nic", Run: func(ctx context.Context, d *dev.Device) error {
		n := &testNIC{gen: int(gen.Add(1)), rx: make(chan []byte, 1), sent: make(chan []byte, 1), down: make(chan struct{})}
		defer close(n.down)
		d.Ready(n)
		nics <- n
		select {
		case <-ctx.Done():
			return nil
		case <-crash:
			return errors.New("simulated crash") // devmgr restarts the driver
		}
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mgr := dev.NewManager(proc.NewTable(), nil, nil)
	mgr.Start(ctx, []dev.Node{{Name: "net0", Compatible: "test,nic"}})

	name, nic, err := findNIC(ctx, mgr, 5*time.Second)
	if err != nil || name != "net0" {
		t.Fatalf("findNIC: %q %v", name, err)
	}
	first := <-nics
	f := &followNIC{mgr: mgr, name: name, cur: nic}

	first.rx <- []byte("one")
	if got, err := f.Recv(ctx); err != nil || string(got) != "one" {
		t.Fatalf("Recv: %q %v", got, err)
	}

	crash <- struct{}{}
	second := <-nics
	second.rx <- []byte("two")
	if got, err := f.Recv(ctx); err != nil || string(got) != "two" {
		t.Fatalf("Recv after restart: %q %v", got, err)
	}
	if err := f.Send(ctx, []byte("out")); err != nil {
		t.Fatal(err)
	}
	if got := <-second.sent; string(got) != "out" {
		t.Fatalf("sent %q", got)
	}
	if f.MAC()[5] != 2 {
		t.Fatalf("MAC from the old instance: %x", f.MAC())
	}
}

func TestFindNICGivesUp(t *testing.T) {
	mgr := dev.NewManager(proc.NewTable(), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx, nil)
	if _, _, err := findNIC(ctx, mgr, 100*time.Millisecond); err == nil {
		t.Fatal("found a NIC on a board without one")
	}
}
