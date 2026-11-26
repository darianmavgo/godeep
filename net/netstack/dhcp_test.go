package netstack

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// serveDHCP answers DISCOVER with an OFFER and REQUEST with an ACK of
// 10.0.0.50/24, router and name server 10.0.0.1.
func serveDHCP(t *testing.T, ctx context.Context, srv *Stack) {
	var wq waiter.Queue
	ep, terr := srv.s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		t.Fatal(terr)
	}
	ep.SocketOptions().SetBroadcast(true)
	if terr := ep.Bind(tcpip.FullAddress{NIC: nicID, Port: dhcpServerPort}); terr != nil {
		t.Fatal(terr)
	}
	c := gonet.NewUDPConn(&wq, ep)
	context.AfterFunc(ctx, func() { c.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			m, err := parseDHCP(buf[:n])
			if err != nil || m.op != opRequest {
				continue
			}
			typ := byte(msgOffer)
			if m.msgType == msgRequest {
				typ = msgAck
			}
			r := make([]byte, 240)
			r[0], r[1], r[2] = opReply, 1, 6
			copy(r[4:8], m.xid[:])
			copy(r[16:20], []byte{10, 0, 0, 50})
			binary.BigEndian.PutUint32(r[236:], magicCookie)
			r = append(r, optMsgType, 1, typ, optSubnet, 4, 255, 255, 255, 0, optRouter, 4, 10, 0, 0, 1,
				optDNS, 4, 10, 0, 0, 1, optServerID, 4, 10, 0, 0, 1, optLease, 4, 0, 0, 0x0e, 0x10, optEnd)
			c.WriteTo(r, &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpClientPort})
		}
	}()
}

// serveDNS answers A queries for *.test with 10.0.0.1.
func serveDNS(t *testing.T, ctx context.Context, srv *Stack) {
	c, err := gonet.DialUDP(srv.s, &tcpip.FullAddress{NIC: nicID, Port: 53}, nil, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	context.AfterFunc(ctx, func() { c.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			var q dnsmessage.Message
			if q.Unpack(buf[:n]) != nil || len(q.Questions) != 1 {
				continue
			}
			r := dnsmessage.Message{Header: dnsmessage.Header{ID: q.Header.ID, Response: true}, Questions: q.Questions}
			if strings.HasSuffix(q.Questions[0].Name.String(), ".test.") {
				r.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					Body:   &dnsmessage.AResource{A: [4]byte{10, 0, 0, 1}},
				}}
			} else {
				r.Header.RCode = dnsmessage.RCodeNameError
			}
			b, _ := r.Pack()
			c.WriteTo(b, from)
		}
	}()
}

func TestDHCPAndDNS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	var wg sync.WaitGroup
	a, b := cable()
	srv := run(t, ctx, &wg, a, "10.0.0.1/24")
	cli, err := New(b, Config{})
	if err != nil {
		t.Fatal(err)
	}
	wg.Add(2)
	go func() { defer wg.Done(); cli.RunRX(ctx) }()
	go func() { defer wg.Done(); cli.RunTX(ctx) }()
	defer func() {
		cancel()
		wg.Wait()
		srv.Close()
		cli.Close()
	}()
	serveDHCP(t, ctx, srv)
	serveDNS(t, ctx, srv)

	lease, err := cli.DHCP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Addr: netip.MustParsePrefix("10.0.0.50/24"), Gateway: netip.MustParseAddr("10.0.0.1"),
		DNS: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}
	if lease.Addr != want.Addr || lease.Gateway != want.Gateway || len(lease.DNS) != 1 || lease.DNS[0] != want.DNS[0] || lease.Time != time.Hour {
		t.Fatalf("lease %+v", lease)
	}
	if d := cli.Devices()[0]; !strings.Contains(d, "10.0.0.50/24") || !strings.Contains(d, "dhcp from 10.0.0.1") || !strings.Contains(d, "dns 10.0.0.1") {
		t.Fatalf("Devices: %s", d)
	}

	addrs, err := cli.LookupHost(ctx, "server.test")
	if err != nil || len(addrs) != 1 || addrs[0] != netip.MustParseAddr("10.0.0.1") {
		t.Fatalf("lookup: %v %v", addrs, err)
	}
	if _, err := cli.LookupHost(ctx, "nope.invalid"); err == nil {
		t.Fatal("NXDOMAIN resolved")
	}

	// Dial by name over the leased address.
	l, err := srv.Listen(ctx, "tcp", ":80")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := l.Accept()
		if err == nil {
			io.WriteString(c, "hello by name")
			c.Close()
		}
	}()
	c, err := cli.Dial(ctx, "tcp", "server.test:80")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(c)
	c.Close()
	if string(got) != "hello by name" {
		t.Fatalf("%q", got)
	}
}
