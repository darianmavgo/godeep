package netstack

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// DHCPv4 (RFC 2131): DISCOVER, OFFER, REQUEST, ACK, then the lease's
// address, router and name servers configure the interface. No renewal
// yet: leases (QEMU's last a day) are assumed to outlive the session.

// DHCP message fields and options.
const (
	dhcpServerPort = 67
	dhcpClientPort = 68
	opRequest      = 1
	opReply        = 2
	magicCookie    = 0x63825363
	flagBroadcast  = 0x8000

	optSubnet    = 1
	optRouter    = 3
	optDNS       = 6
	optReqIP     = 50
	optLease     = 51
	optMsgType   = 53
	optServerID  = 54
	optParamList = 55
	optEnd       = 255

	msgDiscover = 1
	msgOffer    = 2
	msgRequest  = 3
	msgAck      = 5
	msgNak      = 6
)

// Lease is what a DHCP server granted.
type Lease struct {
	Config
	Server netip.Addr
	Time   time.Duration
}

// DHCP gets a lease and configures the interface with it. It tries for as
// long as ctx allows (retrying every second).
func (st *Stack) DHCP(ctx context.Context) (Lease, error) {
	// Until there's an address: send from 0.0.0.0, accept the server's
	// replies to an address that isn't ours yet (promiscuous), route
	// broadcasts out of the NIC.
	st.s.SetPromiscuousMode(nicID, true)
	defer st.s.SetPromiscuousMode(nicID, false)
	st.mu.Lock()
	configured := st.cfg.Addr.IsValid()
	if !configured {
		st.s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{Address: header.IPv4Any, PrefixLen: 0}},
			stack.AddressProperties{PEB: stack.CanBePrimaryEndpoint})
		st.s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})
	}
	st.mu.Unlock()
	if !configured {
		defer st.s.RemoveAddress(nicID, header.IPv4Any)
	}

	var wq waiter.Queue
	ep, terr := st.s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		return Lease{}, fmt.Errorf("dhcp: %s", terr)
	}
	ep.SocketOptions().SetBroadcast(true)
	if terr := ep.Bind(tcpip.FullAddress{NIC: nicID, Port: dhcpClientPort}); terr != nil {
		ep.Close()
		return Lease{}, fmt.Errorf("dhcp: bind: %s", terr)
	}
	conn := gonet.NewUDPConn(&wq, ep)
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	mac := st.nic.MAC()
	var xid [4]byte
	rand.Read(xid[:])
	bcast := &tcpip.FullAddress{NIC: nicID, Addr: header.IPv4Broadcast, Port: dhcpServerPort}

	exchange := func(msg []byte, want byte) (*dhcpMsg, error) {
		buf := make([]byte, 1500)
		for {
			if _, err := conn.WriteTo(msg, fullToUDP(bcast)); err != nil {
				return nil, err
			}
			conn.SetReadDeadline(time.Now().Add(time.Second))
			for {
				n, _, err := conn.ReadFrom(buf)
				if err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					break // timeout: send again
				}
				m, err := parseDHCP(buf[:n])
				if err != nil || m.op != opReply || m.xid != xid {
					continue
				}
				if m.msgType == msgNak {
					return nil, errors.New("dhcp: server refused (NAK)")
				}
				if m.msgType == want {
					return m, nil
				}
			}
		}
	}

	offer, err := exchange(buildDHCP(xid, mac, msgDiscover, netip.Addr{}, netip.Addr{}), msgOffer)
	if err != nil {
		return Lease{}, err
	}
	ack, err := exchange(buildDHCP(xid, mac, msgRequest, offer.yiaddr, offer.server), msgAck)
	if err != nil {
		return Lease{}, err
	}
	l := Lease{Server: ack.server, Time: ack.lease, Config: Config{
		Addr: netip.PrefixFrom(ack.yiaddr, ack.prefix), Gateway: ack.router, DNS: ack.dns}}
	if st.isPinned() {
		return l, errors.New("dhcp: the interface is pinned to a static setup")
	}
	if err := st.Configure(l.Config, fmt.Sprintf("dhcp from %s, lease %v", l.Server, l.Time)); err != nil {
		return Lease{}, err
	}
	return l, nil
}

func fullToUDP(a *tcpip.FullAddress) *net.UDPAddr {
	b := a.Addr.As4()
	return &net.UDPAddr{IP: net.IPv4(b[0], b[1], b[2], b[3]), Port: int(a.Port)}
}

func buildDHCP(xid [4]byte, mac [6]byte, typ byte, reqIP, server netip.Addr) []byte {
	b := make([]byte, 240, 300)
	b[0], b[1], b[2] = opRequest, 1, 6 // Ethernet, 6-byte addresses
	copy(b[4:8], xid[:])
	binary.BigEndian.PutUint16(b[10:], flagBroadcast) // the server broadcasts replies
	copy(b[28:34], mac[:])
	binary.BigEndian.PutUint32(b[236:], magicCookie)
	b = append(b, optMsgType, 1, typ)
	if reqIP.IsValid() {
		a := reqIP.As4()
		b = append(b, optReqIP, 4, a[0], a[1], a[2], a[3])
	}
	if server.IsValid() {
		a := server.As4()
		b = append(b, optServerID, 4, a[0], a[1], a[2], a[3])
	}
	b = append(b, optParamList, 4, optSubnet, optRouter, optDNS, optLease, optEnd)
	return b
}

type dhcpMsg struct {
	op      byte
	xid     [4]byte
	yiaddr  netip.Addr
	msgType byte
	prefix  int
	router  netip.Addr
	dns     []netip.Addr
	server  netip.Addr
	lease   time.Duration
}

func parseDHCP(b []byte) (*dhcpMsg, error) {
	if len(b) < 240 || binary.BigEndian.Uint32(b[236:]) != magicCookie {
		return nil, errors.New("not DHCP")
	}
	m := &dhcpMsg{op: b[0], yiaddr: netip.AddrFrom4([4]byte(b[16:20])), prefix: 24}
	copy(m.xid[:], b[4:8])
	opts := b[240:]
	for len(opts) > 0 {
		code := opts[0]
		if code == optEnd {
			break
		}
		if code == 0 { // pad
			opts = opts[1:]
			continue
		}
		if len(opts) < 2 || len(opts) < 2+int(opts[1]) {
			return nil, errors.New("truncated option")
		}
		v := opts[2 : 2+int(opts[1])]
		opts = opts[2+int(opts[1]):]
		switch {
		case code == optMsgType && len(v) == 1:
			m.msgType = v[0]
		case code == optSubnet && len(v) == 4:
			ones := 0
			for x := binary.BigEndian.Uint32(v); x&0x80000000 != 0; x <<= 1 {
				ones++
			}
			m.prefix = ones
		case code == optRouter && len(v) >= 4:
			m.router = netip.AddrFrom4([4]byte(v[:4]))
		case code == optDNS:
			for ; len(v) >= 4; v = v[4:] {
				m.dns = append(m.dns, netip.AddrFrom4([4]byte(v[:4])))
			}
		case code == optServerID && len(v) == 4:
			m.server = netip.AddrFrom4([4]byte(v))
		case code == optLease && len(v) == 4:
			m.lease = time.Duration(binary.BigEndian.Uint32(v)) * time.Second
		}
	}
	return m, nil
}
