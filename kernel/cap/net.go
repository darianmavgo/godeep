// Package cap holds godeep's capability interfaces: authority a Proc is
// handed as an ordinary Go value, instead of reaching for a global.
package cap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
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

// PacketNet is a Net that also receives datagrams on a port of its own
// ("udp"): the datagram counterpart of Listen. (Dial "udp" sends to one
// peer.)
type PacketNet interface {
	ListenPacket(ctx context.Context, network, addr string) (net.PacketConn, error)
}

// ListenPacket opens a datagram port on n, if n is a PacketNet.
func ListenPacket(ctx context.Context, n Net, network, addr string) (net.PacketConn, error) {
	pn, ok := n.(PacketNet)
	if !ok {
		return nil, fmt.Errorf("listen %s: this network has no datagram ports", network)
	}
	return pn.ListenPacket(ctx, network, addr)
}

// Pinger is a Net that sends ICMP echo requests: one request of size
// bytes to host, and the reply's round-trip time.
type Pinger interface {
	Ping(ctx context.Context, host string, seq uint16, size int) (netip.Addr, time.Duration, error)
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

func (p *PendingNet) ListenPacket(ctx context.Context, network, addr string) (net.PacketConn, error) {
	n, err := p.Wait(ctx)
	if err != nil {
		return nil, err
	}
	return ListenPacket(ctx, n, network, addr)
}

func (p *PendingNet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	n, err := p.Wait(ctx)
	if err != nil {
		return nil, err
	}
	return n.Dial(ctx, network, addr)
}

// ErrPermission is returned by a narrowed capability for what it withholds.
var ErrPermission = errors.New("not permitted by this capability")

// Restricted is a narrowed Net: the same network without some of its
// authority. It is what a less trusted Proc (an SSH session, a `restrict`ed
// command) is handed instead of the Net itself.
type Restricted struct {
	N        Net
	NoListen bool // Listen fails: no services offered to the network
	NoDial   bool // Dial fails: no outgoing connections
}

// DialOnly returns n without the authority to listen.
func DialOnly(n Net) Net {
	if n == nil {
		return nil
	}
	return &Restricted{N: n, NoListen: true}
}

func (r *Restricted) Listen(ctx context.Context, network, addr string) (net.Listener, error) {
	if r.NoListen {
		return nil, fmt.Errorf("listen %s: %w", addr, ErrPermission)
	}
	return r.N.Listen(ctx, network, addr)
}

// ListenPacket opens a datagram port: a service offered to the network,
// so it needs the authority to listen.
func (r *Restricted) ListenPacket(ctx context.Context, network, addr string) (net.PacketConn, error) {
	if r.NoListen {
		return nil, fmt.Errorf("listen %s: %w", addr, ErrPermission)
	}
	return ListenPacket(ctx, r.N, network, addr)
}

func (r *Restricted) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if r.NoDial {
		return nil, fmt.Errorf("dial %s: %w", addr, ErrPermission)
	}
	return r.N.Dial(ctx, network, addr)
}

// LookupHost resolves through the underlying network, if it resolves names
// (and dialing is allowed: a name lookup is traffic).
func (r *Restricted) LookupHost(ctx context.Context, name string) ([]netip.Addr, error) {
	if r.NoDial {
		return nil, fmt.Errorf("lookup %s: %w", name, ErrPermission)
	}
	n, err := Ready(ctx, r.N)
	if err != nil {
		return nil, err
	}
	res, ok := n.(Resolver)
	if !ok {
		return nil, errors.New("this network doesn't resolve names")
	}
	return res.LookupHost(ctx, name)
}

// Ping goes through the underlying network if dialing is allowed (an echo
// request is outgoing traffic).
func (r *Restricted) Ping(ctx context.Context, host string, seq uint16, size int) (netip.Addr, time.Duration, error) {
	if r.NoDial {
		return netip.Addr{}, 0, fmt.Errorf("ping %s: %w", host, ErrPermission)
	}
	n, err := Ready(ctx, r.N)
	if err != nil {
		return netip.Addr{}, 0, err
	}
	p, ok := n.(Pinger)
	if !ok {
		return netip.Addr{}, 0, errors.New("this network can't ping")
	}
	return p.Ping(ctx, host, seq, size)
}

// Unwrap is the network underneath, for describing it (ifconfig), not for
// using it.
func (r *Restricted) Unwrap() Net { return r.N }

// Ready waits for a PendingNet to resolve; any other Net is returned as is.
func Ready(ctx context.Context, n Net) (Net, error) {
	if p, ok := n.(*PendingNet); ok {
		return p.Wait(ctx)
	}
	return n, nil
}

// Underlying peels Restricted wrappers and resolves a PendingNet: the
// network a capability is ultimately backed by.
func Underlying(ctx context.Context, n Net) (Net, error) {
	for {
		u, ok := n.(interface{ Unwrap() Net })
		if !ok {
			break
		}
		n = u.Unwrap()
	}
	return Ready(ctx, n)
}
