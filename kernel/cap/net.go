// Package cap holds godeep's capability interfaces: authority a Proc is
// handed as an ordinary Go value, instead of reaching for a global.
package cap

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
)

// Net is the authority to use a network: listen and dial. On QEMU and the
// Mac it is backed by godeep's TCP/IP stack (net/netstack); in the hosted
// build, by the host's sockets.
type Net interface {
	Listen(ctx context.Context, network, addr string) (net.Listener, error)
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
}

// Resolver is a Net that resolves names (DNS).
type Resolver interface {
	LookupHost(ctx context.Context, name string) ([]netip.Addr, error)
}

// ErrNoNetwork is returned when the system has no network to hand out.
var ErrNoNetwork = errors.New("no network")

// PendingNet is a Net that becomes usable later: the kernel hands it out at
// boot, before the network device and the stack exist, and Resolve fills it
// in. Listen and Dial wait (or return ctx's error) until then.
type PendingNet struct {
	once  sync.Once
	ready chan struct{}
	n     Net
	err   error
}

func NewPendingNet() *PendingNet { return &PendingNet{ready: make(chan struct{})} }

// Resolve sets the network (or the error saying why there is none). Only the
// first call counts.
func (p *PendingNet) Resolve(n Net, err error) {
	p.once.Do(func() {
		if n == nil && err == nil {
			err = ErrNoNetwork
		}
		p.n, p.err = n, err
		close(p.ready)
	})
}

// Wait returns the network once resolved.
func (p *PendingNet) Wait(ctx context.Context) (Net, error) {
	select {
	case <-p.ready:
		return p.n, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *PendingNet) Listen(ctx context.Context, network, addr string) (net.Listener, error) {
	n, err := p.Wait(ctx)
	if err != nil {
		return nil, err
	}
	return n.Listen(ctx, network, addr)
}

func (p *PendingNet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	n, err := p.Wait(ctx)
	if err != nil {
		return nil, err
	}
	return n.Dial(ctx, network, addr)
}
