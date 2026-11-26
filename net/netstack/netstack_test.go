package netstack

import (
	"bytes"
	"context"
	"io"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// wire is one end of a simulated Ethernet cable.
type wire struct {
	mac [6]byte
	in  chan []byte
	out chan []byte
}

func (w *wire) MAC() [6]byte { return w.mac }
func (w *wire) MTU() int     { return 1500 }
func (w *wire) Send(ctx context.Context, f []byte) error {
	select {
	case w.out <- append([]byte(nil), f...):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *wire) Recv(ctx context.Context) ([]byte, error) {
	select {
	case f := <-w.in:
		return f, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func cable() (*wire, *wire) {
	a, b := make(chan []byte, 64), make(chan []byte, 64)
	return &wire{mac: [6]byte{2, 0, 0, 0, 0, 1}, in: a, out: b}, &wire{mac: [6]byte{2, 0, 0, 0, 0, 2}, in: b, out: a}
}

func run(t *testing.T, ctx context.Context, wg *sync.WaitGroup, nic NIC, addr string) *Stack {
	t.Helper()
	st, err := New(nic, Config{Addr: netip.MustParsePrefix(addr)})
	if err != nil {
		t.Fatal(err)
	}
	wg.Add(2)
	go func() { defer wg.Done(); st.RunRX(ctx) }()
	go func() { defer wg.Done(); st.RunTX(ctx) }()
	return st
}

func TestTCPOverACable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	var wg sync.WaitGroup
	a, b := cable()
	srv := run(t, ctx, &wg, a, "10.0.0.1/24")
	cli := run(t, ctx, &wg, b, "10.0.0.2/24")
	defer func() {
		cancel()
		wg.Wait()
		srv.Close()
		cli.Close()
	}()

	l, err := srv.Listen(ctx, "tcp", ":22")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	echoed := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			echoed <- err
			return
		}
		defer c.Close()
		_, err = io.Copy(c, c)
		echoed <- err
	}()

	c, err := cli.Dial(ctx, "tcp", "10.0.0.1:22")
	if err != nil {
		t.Fatal(err)
	}
	msg := bytes.Repeat([]byte("godeep "), 20000) // 140 KB: many segments, window updates
	go func() { c.Write(msg); c.(interface{ CloseWrite() error }).CloseWrite() }()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("echo: got %d bytes, want %d", len(got), len(msg))
	}
	c.Close()
	if err := <-echoed; err != nil {
		t.Fatal(err)
	}
	if srv.rxFrames.Load() == 0 || srv.txFrames.Load() == 0 {
		t.Fatal("no frames counted")
	}
}

func TestListenerClosesWithContext(t *testing.T) {
	a, _ := cable()
	st, err := New(a, Config{Addr: netip.MustParsePrefix("10.0.0.1/24")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	l, err := st.Listen(ctx, "tcp", ":22")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := l.Accept(); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Accept returned a connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept still blocked after cancel")
	}
}

func TestAddresses(t *testing.T) {
	a, _ := cable()
	st, err := New(a, Config{Addr: netip.MustParsePrefix("10.0.2.15/24")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Dial(context.Background(), "tcp", "example.com:80"); err == nil || !strings.Contains(err.Error(), "no name servers") {
		t.Fatalf("a name without name servers: %v", err)
	}
	if _, err := st.Listen(context.Background(), "udp", ":53"); err == nil {
		t.Fatal("udp listen accepted")
	}
	if got := st.Devices()[0]; got[:13] != "10.0.2.15/24 " {
		t.Fatalf("Devices: %q", got)
	}
}
