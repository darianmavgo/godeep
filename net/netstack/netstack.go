// Package netstack runs gVisor's TCP/IP stack over one Ethernet device.
//
// The device is anything that sends and receives Ethernet frames (NIC):
// virtio-net on QEMU, later USB gadget Ethernet (CDC-ECM) on the Mac. Frames
// move between the device and gVisor through a channel endpoint; two pump
// loops (RunRX, RunTX) do the copying, and the caller runs them in its own
// Proc so they are accounted and killable.
//
// The Stack is a cap.Net: Listen and Dial return ordinary net.Listener and
// net.Conn values (gVisor's gonet adapters). Nothing here touches tamago's
// global net.SocketFunc hook, so the network stays a capability.
package netstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"godeep/kernel/netdev"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// NIC is the device the stack runs on.
type NIC = netdev.NIC

// Config is the interface's IPv4 setup: static (Addr set), or from DHCP.
type Config struct {
	Addr    netip.Prefix // e.g. 10.0.2.15/24; invalid: none (DHCP sets it)
	Gateway netip.Addr   // default route; invalid: none
	DNS     []netip.Addr // name servers
}

// QEMUUser is QEMU's user-mode network ("-netdev user"): the guest is
// 10.0.2.15, the host's NAT gateway 10.0.2.2, DNS 10.0.2.3. (Its DHCP
// server hands out the same; this is the fallback when DHCP fails.)
var QEMUUser = Config{
	Addr:    netip.MustParsePrefix("10.0.2.15/24"),
	Gateway: netip.MustParseAddr("10.0.2.2"),
	DNS:     []netip.Addr{netip.MustParseAddr("10.0.2.3")},
}

const nicID = 1

// Stack is a TCP/IP stack on one NIC.
type Stack struct {
	nic NIC
	s   *stack.Stack
	ch  *channel.Endpoint

	mu     sync.Mutex
	cfg    Config
	source string // "static" or "dhcp, lease ..."
	pinned bool   // a static setup DHCP must not replace (Pin)

	rxFrames, txFrames, rxBytes, txBytes, rxErrors, txErrors atomic.Uint64
}

// New builds the stack: IPv4 + ARP, TCP, UDP, ICMP echo, one interface,
// configured with cfg if it has an address (else call DHCP or Configure).
// Start RunRX and RunTX to move frames.
func New(nic NIC, cfg Config) (*Stack, error) {
	if cfg.Addr.IsValid() && !cfg.Addr.Addr().Is4() {
		return nil, errors.New("netstack: need an IPv4 address")
	}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	mac := nic.MAC()
	ch := channel.New(256, uint32(nic.MTU()), tcpip.LinkAddress(mac[:]))
	if err := s.CreateNIC(nicID, ethernet.NewWithNetworkMTU(ch)); err != nil {
		s.Close()
		return nil, fmt.Errorf("netstack: create NIC: %s", err)
	}
	st := &Stack{nic: nic, s: s, ch: ch}
	if cfg.Addr.IsValid() {
		if err := st.Configure(cfg, "static"); err != nil {
			s.Close()
			return nil, err
		}
	}
	return st, nil
}

// Configure replaces the interface's address, routes and name servers.
func (st *Stack) Configure(cfg Config, source string) error {
	if !cfg.Addr.Addr().Is4() {
		return errors.New("netstack: need an IPv4 address")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	pa := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(cfg.Addr.Addr().As4()), PrefixLen: cfg.Addr.Bits()},
	}
	// The same address again (a DHCP renewal): keep it, so open
	// connections survive; only routes and name servers are refreshed.
	if cfg.Addr != st.cfg.Addr {
		if old := st.cfg.Addr; old.IsValid() {
			st.s.RemoveAddress(nicID, tcpip.AddrFrom4(old.Addr().As4()))
		}
		if err := st.s.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err != nil {
			return fmt.Errorf("netstack: add address: %s", err)
		}
	}
	routes := []tcpip.Route{{Destination: pa.AddressWithPrefix.Subnet(), NIC: nicID}}
	if cfg.Gateway.Is4() {
		routes = append(routes, tcpip.Route{Destination: header.IPv4EmptySubnet,
			Gateway: tcpip.AddrFrom4(cfg.Gateway.As4()), NIC: nicID})
	}
	st.s.SetRouteTable(routes)
	st.cfg, st.source = cfg, source
	return nil
}

// Config returns the current setup.
// Pin configures cfg and keeps it: later DHCP leases (renewals, retries)
// no longer change the interface.
func (st *Stack) Pin(cfg Config, source string) error {
	if err := st.Configure(cfg, source); err != nil {
		return err
	}
	st.mu.Lock()
	st.pinned = true
	st.mu.Unlock()
	return nil
}

func (st *Stack) isPinned() bool { st.mu.Lock(); defer st.mu.Unlock(); return st.pinned }

func (st *Stack) Config() Config { st.mu.Lock(); defer st.mu.Unlock(); return st.cfg }

// RunRX delivers received frames to the stack until ctx is done.
func (st *Stack) RunRX(ctx context.Context) error {
	for {
		frame, err := st.nic.Recv(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if len(frame) < header.EthernetMinimumSize {
			st.rxErrors.Add(1)
			continue
		}
		st.rxFrames.Add(1)
		st.rxBytes.Add(uint64(len(frame)))
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
		st.ch.InjectInbound(0, pkt) // the ethernet endpoint reads the type
		pkt.DecRef()
	}
}

// RunTX sends the stack's outgoing frames until ctx is done.
func (st *Stack) RunTX(ctx context.Context) error {
	var frame []byte
	for {
		pkt := st.ch.ReadContext(ctx)
		if pkt == nil {
			return nil // ctx done
		}
		frame = frame[:0]
		for _, s := range pkt.AsSlices() {
			frame = append(frame, s...)
		}
		pkt.DecRef()
		if err := st.nic.Send(ctx, frame); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			st.txErrors.Add(1)
			continue
		}
		st.txFrames.Add(1)
		st.txBytes.Add(uint64(len(frame)))
	}
}

// Close tears the stack down: open connections are reset.
func (st *Stack) Close() {
	st.ch.Close()
	st.s.Close()
}

// Devices is the stack's lsdev view.
func (st *Stack) Devices() []string {
	mac := st.nic.MAC()
	st.mu.Lock()
	cfg, source := st.cfg, st.source
	st.mu.Unlock()
	gw, addr := "none", "no address"
	if cfg.Gateway.IsValid() {
		gw = cfg.Gateway.String()
	}
	if cfg.Addr.IsValid() {
		addr = cfg.Addr.String()
	}
	dns := "none"
	if len(cfg.DNS) > 0 {
		var ds []string
		for _, d := range cfg.DNS {
			ds = append(ds, d.String())
		}
		dns = strings.Join(ds, ",")
	}
	return []string{fmt.Sprintf("%s (%s, %s), gateway %s, dns %s, mtu %d: rx %d frames %d B (%d bad), tx %d frames %d B (%d errors)",
		addr, net.HardwareAddr(mac[:]), source, gw, dns, st.nic.MTU(),
		st.rxFrames.Load(), st.rxBytes.Load(), st.rxErrors.Load(),
		st.txFrames.Load(), st.txBytes.Load(), st.txErrors.Load())}
}

// Addr is the interface's address.
func (st *Stack) Addr() netip.Prefix { return st.Config().Addr }

func (st *Stack) fullAddr(ctx context.Context, addr string, listen bool) (tcpip.FullAddress, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return tcpip.FullAddress{}, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return tcpip.FullAddress{}, fmt.Errorf("bad port %q", port)
	}
	fa := tcpip.FullAddress{NIC: nicID, Port: uint16(p)}
	if host == "" {
		if !listen {
			return fa, errors.New("dial: missing host")
		}
		return fa, nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		if listen {
			return fa, fmt.Errorf("%q: listen on an IPv4 address", host)
		}
		addrs, err := st.LookupHost(ctx, host)
		if err != nil {
			return fa, err
		}
		ip = addrs[0]
	}
	if !ip.Is4() {
		return fa, fmt.Errorf("%q: only IPv4", host)
	}
	if !ip.IsUnspecified() {
		fa.Addr = tcpip.AddrFrom4(ip.As4())
	}
	return fa, nil
}

// Listen implements cap.Net ("tcp" or "tcp4"). The listener closes when ctx
// is done, so a blocked Accept returns and its Proc can exit.
func (st *Stack) Listen(ctx context.Context, network, addr string) (net.Listener, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("listen %s: unsupported network", network)
	}
	fa, err := st.fullAddr(ctx, addr, true)
	if err != nil {
		return nil, err
	}
	l, err := gonet.ListenTCP(st.s, fa, ipv4.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	return closeOnDone(ctx, l), nil
}

// Dial implements cap.Net ("tcp", "tcp4", "udp" or "udp4": a UDP socket
// connected to addr).
func (st *Stack) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "udp", "udp4":
	default:
		return nil, fmt.Errorf("dial %s: unsupported network", network)
	}
	fa, err := st.fullAddr(ctx, addr, false)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(network, "udp") {
		c, err := gonet.DialUDP(st.s, nil, &fa, ipv4.ProtocolNumber)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	return gonet.DialContextTCP(ctx, st.s, fa, ipv4.ProtocolNumber)
}

// ListenPacket implements cap.PacketNet ("udp" or "udp4"): a UDP port that
// receives from anyone. It closes when ctx is done.
func (st *Stack) ListenPacket(ctx context.Context, network, addr string) (net.PacketConn, error) {
	if network != "udp" && network != "udp4" {
		return nil, fmt.Errorf("listen %s: unsupported network", network)
	}
	fa, err := st.fullAddr(ctx, addr, true)
	if err != nil {
		return nil, err
	}
	c, err := gonet.DialUDP(st.s, &fa, nil, ipv4.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	context.AfterFunc(ctx, func() { c.Close() })
	return c, nil
}

// listener closes the wrapped listener when ctx is done. The watcher is
// started with context.AfterFunc, which needs no goroutine until ctx ends.
type listener struct {
	net.Listener
	stop func() bool
}

func closeOnDone(ctx context.Context, l net.Listener) net.Listener {
	return &listener{Listener: l, stop: context.AfterFunc(ctx, func() { l.Close() })}
}

func (l *listener) Close() error {
	l.stop()
	return l.Listener.Close()
}
