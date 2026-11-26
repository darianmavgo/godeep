package netstack

import (
	"testing"
	"time"
)

func TestSNTP(t *testing.T) {
	t1 := time.Unix(1_800_000_000, 123_456_000)
	req := SNTPRequest(t1)
	if req[0] != 0x23 || len(req) != 48 {
		t.Fatalf("request %x", req[:4])
	}
	// a server 10 s ahead, 20 ms each way
	srv := 10 * time.Second
	t2 := t1.Add(20*time.Millisecond + srv)
	t3 := t2.Add(time.Millisecond)
	t4 := t3.Add(-srv + 20*time.Millisecond)
	resp := make([]byte, 48)
	resp[0], resp[1] = 4<<3|4, 2
	copy(resp[24:], req[40:48])
	putNTPTime(resp[32:], t2)
	putNTPTime(resp[40:], t3)
	off, rtt, err := SNTPResponse(resp, t1, t4)
	if err != nil {
		t.Fatal(err)
	}
	if d := off - srv; d < -time.Microsecond || d > time.Microsecond {
		t.Fatalf("offset %v, want %v", off, srv)
	}
	if d := rtt - 40*time.Millisecond; d < -time.Microsecond || d > time.Microsecond {
		t.Fatalf("rtt %v", rtt)
	}
	if _, _, err := SNTPResponse(resp, t1.Add(time.Second), t4); err == nil {
		t.Fatal("accepted a reply to another request")
	}
	resp[1] = 0
	if _, _, err := SNTPResponse(resp, t1, t4); err == nil {
		t.Fatal("accepted kiss-o'-death")
	}
}

func TestLinkLocal(t *testing.T) {
	a := LinkLocal([6]byte{0x52, 0x54, 0, 0x12, 0x34, 0x56})
	b := LinkLocal([6]byte{0x52, 0x54, 0, 0x12, 0x34, 0x57})
	for _, c := range []Config{a, b} {
		ip := c.Addr.Addr().As4()
		if ip[0] != 169 || ip[1] != 254 || ip[2] == 0 || ip[2] == 255 || c.Addr.Bits() != 16 || c.Gateway.IsValid() {
			t.Fatalf("%v", c)
		}
	}
	if a.Addr == b.Addr {
		t.Fatal("two MACs, one address")
	}
	if LinkLocal([6]byte{1}).Addr != LinkLocal([6]byte{1}).Addr {
		t.Fatal("not stable")
	}
}
