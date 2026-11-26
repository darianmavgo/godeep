package netstack

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// LookupHost resolves a name to IPv4 addresses with the configured name
// servers (A records over UDP; CNAMEs are followed by the server's answer).
// An address literal is returned as is.
func (st *Stack) LookupHost(ctx context.Context, name string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(name); err == nil {
		return []netip.Addr{ip}, nil
	}
	servers := st.Config().DNS
	if len(servers) == 0 {
		return nil, fmt.Errorf("lookup %s: no name servers", name)
	}
	q, err := dnsmessage.NewName(dnsName(name))
	if err != nil {
		return nil, fmt.Errorf("lookup %s: %w", name, err)
	}
	var last error
	for try := 0; try < 3; try++ {
		for _, srv := range servers {
			addrs, err := st.query(ctx, srv, q)
			if err == nil {
				return addrs, nil
			}
			last = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("lookup %s: %w", name, last)
}

func dnsName(n string) string {
	if n == "" || n[len(n)-1] != '.' {
		n += "."
	}
	return n
}

func (st *Stack) query(ctx context.Context, srv netip.Addr, q dnsmessage.Name) ([]netip.Addr, error) {
	conn, err := gonet.DialUDP(st.s, nil, &tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(srv.As4()), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	id := uint16(time.Now().UnixNano())
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: q, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	b, err := msg.Pack()
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(b); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		var r dnsmessage.Message
		if r.Unpack(buf[:n]) != nil || r.Header.ID != id || !r.Header.Response {
			continue
		}
		if r.Header.RCode != dnsmessage.RCodeSuccess {
			return nil, fmt.Errorf("%s", r.Header.RCode)
		}
		var out []netip.Addr
		for _, a := range r.Answers {
			if rr, ok := a.Body.(*dnsmessage.AResource); ok {
				out = append(out, netip.AddrFrom4(rr.A))
			}
		}
		if len(out) == 0 {
			return nil, errors.New("no address records")
		}
		return out, nil
	}
}
