// Package hostnet is cap.Net for the hosted build: the host's own sockets.
//
// Listening is confined to the loopback interface, and privileged ports
// (below 1024) move up by PortOffset, so godeep's sshd on ":22" listens on
// 127.0.0.1:2222: the same address QEMU's hostfwd gives the guest's port 22.
package hostnet

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// PortOffset is added to ports below 1024.
const PortOffset = 2200

// Net is the host network.
type Net struct{}

// HostAddr is where Listen(addr) really listens.
func HostAddr(addr string) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return "", fmt.Errorf("bad port %q", port)
	}
	if p > 0 && p < 1024 {
		p += PortOffset
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(p)), nil
}

// Listen listens on the loopback interface; the listener closes when ctx is
// done.
func (Net) Listen(ctx context.Context, network, addr string) (net.Listener, error) {
	a, err := HostAddr(addr)
	if err != nil {
		return nil, err
	}
	l, err := (&net.ListenConfig{}).Listen(ctx, network, a)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { l.Close() })
	return &listener{l, stop}, nil
}

// ListenPacket opens a UDP port on the loopback interface (low ports moved
// like Listen's); it closes when ctx is done.
func (Net) ListenPacket(ctx context.Context, network, addr string) (net.PacketConn, error) {
	a, err := HostAddr(addr)
	if err != nil {
		return nil, err
	}
	c, err := (&net.ListenConfig{}).ListenPacket(ctx, network, a)
	if err != nil {
		return nil, err
	}
	context.AfterFunc(ctx, func() { c.Close() })
	return c, nil
}

func (Net) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

// LookupHost resolves with the host's resolver.
func (Net) LookupHost(ctx context.Context, name string) ([]netip.Addr, error) {
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", name)
	return ips, err
}

// Devices is the lsdev/ifconfig view.
func (Net) Devices() []string {
	return []string{fmt.Sprintf("host network: listens on 127.0.0.1, ports below 1024 + %d", PortOffset)}
}

type listener struct {
	net.Listener
	stop func() bool
}

func (l *listener) Close() error { l.stop(); return l.Listener.Close() }
