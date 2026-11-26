package netstack

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// SNTP (RFC 4330): one request, one answer, the clock offset from the four
// timestamps. Enough to put a date on a machine with no RTC; no
// filtering or slewing.

const ntpEpoch = 2208988800 // seconds from 1900 to 1970

func ntpTime(b []byte) time.Time {
	sec := int64(binary.BigEndian.Uint32(b)) - ntpEpoch
	frac := int64(binary.BigEndian.Uint32(b[4:]))
	return time.Unix(sec, frac*1e9>>32)
}

func putNTPTime(b []byte, t time.Time) {
	binary.BigEndian.PutUint32(b, uint32(t.Unix()+ntpEpoch))
	binary.BigEndian.PutUint32(b[4:], uint32(uint64(t.Nanosecond())<<32/1e9))
}

// SNTPRequest is a client request sent at t1 (version 4, mode 3).
func SNTPRequest(t1 time.Time) []byte {
	b := make([]byte, 48)
	b[0] = 4<<3 | 3
	putNTPTime(b[40:], t1)
	return b
}

// SNTPResponse decodes a server's answer to the request sent at t1 and
// received at t4: the offset to add to the local clock, and the round trip.
func SNTPResponse(b []byte, t1, t4 time.Time) (offset, rtt time.Duration, err error) {
	if len(b) < 48 {
		return 0, 0, errors.New("sntp: short reply")
	}
	if mode := b[0] & 7; mode != 4 && mode != 5 {
		return 0, 0, fmt.Errorf("sntp: mode %d", mode)
	}
	if b[1] == 0 { // stratum 0: kiss-o'-death
		return 0, 0, fmt.Errorf("sntp: server says %q", string(b[12:16]))
	}
	var want [8]byte
	putNTPTime(want[:], t1)
	if string(b[24:32]) != string(want[:]) {
		return 0, 0, errors.New("sntp: reply to another request")
	}
	t2, t3 := ntpTime(b[32:]), ntpTime(b[40:])
	offset = (t2.Sub(t1) + t3.Sub(t4)) / 2
	rtt = t4.Sub(t1) - t3.Sub(t2)
	return offset, rtt, nil
}

// SNTP asks server ("host" or "host:port", port 123 by default) for the
// time and returns the offset to add to time.Now, and the round trip.
func (st *Stack) SNTP(ctx context.Context, server string) (offset, rtt time.Duration, err error) {
	host, port := server, 123
	if h, p, err := net.SplitHostPort(server); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, 0, fmt.Errorf("sntp: bad port %q", p)
		}
		host, port = h, n
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		addrs, err := st.LookupHost(ctx, host)
		if err != nil {
			return 0, 0, err
		}
		ip = addrs[0]
	}
	conn, err := gonet.DialUDP(st.s, nil, &tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(ip.As4()), Port: uint16(port)}, ipv4.ProtocolNumber)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	t1 := time.Now()
	if _, err := conn.Write(SNTPRequest(t1)); err != nil {
		return 0, 0, err
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 128)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return 0, 0, err
		}
		if off, rtt, err := SNTPResponse(buf[:n], t1, time.Now()); err == nil {
			return off, rtt, nil
		}
	}
}

// LinkLocal is the fallback when DHCP fails: an IPv4 link-local address
// (169.254.1.0 - 169.254.254.255, RFC 3927) derived from the MAC, no
// gateway. A computer on the other end of a cable without DHCP picks one
// too, so the two can talk.
func LinkLocal(mac [6]byte) Config {
	h := uint32(2166136261)
	for _, c := range mac {
		h = (h ^ uint32(c)) * 16777619
	}
	third := byte(1 + h%254)
	return Config{Addr: netip.PrefixFrom(netip.AddrFrom4([4]byte{169, 254, third, byte(h >> 8)}), 16)}
}
