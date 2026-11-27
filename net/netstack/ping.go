package netstack

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Ping sends one ICMP echo request with size bytes of data to host (an
// address or a name) and waits for the reply, until ctx is done. It
// implements cap.Pinger: an ICMP "ping socket", like Linux's unprivileged
// ones (the stack picks the identifier).
func (st *Stack) Ping(ctx context.Context, host string, seq uint16, size int) (netip.Addr, time.Duration, error) {
	if size < 0 || size > 65000 {
		return netip.Addr{}, 0, fmt.Errorf("ping: size %d", size)
	}
	fa, err := st.fullAddr(ctx, net.JoinHostPort(host, "0"), false)
	if err != nil {
		return netip.Addr{}, 0, err
	}
	to := netip.AddrFrom4(fa.Addr.As4())
	var wq waiter.Queue
	ep, terr := st.s.NewEndpoint(icmp.ProtocolNumber4, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		return to, 0, fmt.Errorf("ping: %s", terr)
	}
	if terr := ep.Connect(fa); terr != nil {
		ep.Close()
		return to, 0, fmt.Errorf("ping %s: %s", to, terr)
	}
	conn := gonet.NewUDPConn(&wq, ep)
	defer conn.Close()
	if d, ok := ctx.Deadline(); ok {
		conn.SetReadDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Now()) })
	defer stop()

	msg := make([]byte, header.ICMPv4MinimumSize+size)
	h := header.ICMPv4(msg)
	h.SetType(header.ICMPv4Echo)
	h.SetSequence(seq)
	for i := range msg[header.ICMPv4MinimumSize:] {
		msg[header.ICMPv4MinimumSize+i] = byte(i)
	}
	start := time.Now()
	if _, err := conn.Write(msg); err != nil {
		return to, 0, fmt.Errorf("ping %s: %w", to, err)
	}
	buf := make([]byte, len(msg)+64)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return to, 0, ctx.Err()
			}
			return to, 0, fmt.Errorf("ping %s: %w", to, err)
		}
		r := header.ICMPv4(buf[:n])
		if n >= header.ICMPv4MinimumSize && r.Type() == header.ICMPv4EchoReply && r.Sequence() == seq {
			return to, time.Since(start), nil
		}
	}
}
