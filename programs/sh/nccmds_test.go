package sh

import (
	"bufio"
	"context"
	"errors"
	"io/fs"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"godeep/kernel/cap"
	"godeep/net/hostnet"
)

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func TestNCDial(t *testing.T) {
	s := newShell(t)
	s.env.Net = hostnet.Net{}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c) // until nc's half-close
		got <- string(b)
		io.WriteString(c, "reply 1\nreply 2\n")
	}()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	out := s.must("echo hello there | nc 127.0.0.1 " + port + " | wc -l")
	if out != "2\n" {
		t.Fatalf("nc output: %q", out)
	}
	if g := <-got; g != "hello there\n" {
		t.Fatalf("server got %q", g)
	}
	if _, err := s.run("nc 127.0.0.1 " + freePort(t)); err == nil {
		t.Fatal("dial to a closed port")
	}
	if _, err := s.run("restrict -net none nc 127.0.0.1 1"); err == nil {
		t.Fatal("nc without a network")
	}
}

func TestNCListen(t *testing.T) {
	s := newShell(t)
	s.env.Net = hostnet.Net{}
	port := freePort(t)
	s.must("nc -l " + port + " > ram:/got &")
	var c net.Conn
	waitFor(t, func() bool {
		var err error
		c, err = net.Dial("tcp", "127.0.0.1:"+port)
		return err == nil
	})
	io.WriteString(c, "over the wire\n")
	c.Close()
	s.must("wait")
	if got := s.must("cat ram:/got"); got != "over the wire\n" {
		t.Fatalf("received %q", got)
	}
}

// fakePinger answers 10.0.0.2 after 1 ms and nobody else.
type fakePinger struct{ hostnet.Net }

func (fakePinger) Ping(ctx context.Context, host string, seq uint16, size int) (netip.Addr, time.Duration, error) {
	if host == "nowhere" {
		return netip.Addr{}, 0, errors.New("no such host")
	}
	if host != "10.0.0.2" {
		<-ctx.Done()
		return netip.Addr{}, 0, ctx.Err()
	}
	return netip.MustParseAddr(host), time.Millisecond, nil
}

func TestPingCmd(t *testing.T) {
	s := newShell(t)
	s.env.Net = fakePinger{}
	out := s.must("ping -c 3 -i 0.01 -s 10 10.0.0.2")
	sc := bufio.NewScanner(strings.NewReader(out))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 4 || lines[0] != "18 bytes from 10.0.0.2: seq=1 time=1.000ms" ||
		!strings.HasPrefix(lines[3], "--- 10.0.0.2: 3 sent, 3 received, 0% loss") {
		t.Fatalf("ping:\n%s", out)
	}
	if _, err := s.run("ping nowhere"); err == nil || !strings.Contains(err.Error(), "no such host") {
		t.Fatalf("bad host: %v", err)
	}
	if _, err := s.run("restrict -net none ping 10.0.0.2"); err == nil {
		t.Fatal("ping without a network")
	}
	s.env.Net = cap.DialOnly(fakePinger{})
	if out := s.must("ping -c 1 10.0.0.2"); !strings.Contains(out, "1 received") {
		t.Fatalf("dial-only ping: %q", out)
	}
	s.env.Net = &cap.Restricted{N: fakePinger{}, NoDial: true}
	if _, err := s.run("ping -c 1 10.0.0.2"); !errors.Is(err, cap.ErrPermission) {
		t.Fatalf("no-dial ping: %v", err)
	}
}

func TestNCUDP(t *testing.T) {
	s := newShell(t)
	s.env.Net = hostnet.Net{}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() { // echo each datagram back in upper case
		buf := make([]byte, 100)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo([]byte(strings.ToUpper(string(buf[:n]))), from)
		}
	}()
	port := strconv.Itoa(pc.LocalAddr().(*net.UDPAddr).Port)
	start := time.Now()
	if got := s.must("echo hello | nc -u -w 0.3 127.0.0.1 " + port); got != "HELLO\n" {
		t.Fatalf("udp: %q", got)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}

	// listening: print what arrives, answer the sender with stdin
	lport := freeUDPPort(t)
	s.must("nc -u -l " + lport + " > ram:/udp &")
	c, err := net.Dial("udp", "127.0.0.1:"+lport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, func() bool {
		c.Write([]byte("datagram\n"))
		st, err := fs.Stat(mustOpen(t, s, "ram"), "udp")
		return err == nil && st.Size() > 0
	})
	for _, p := range s.env.Table.List() {
		if p.Name == "nc" {
			p.Kill()
		}
	}
	waitFor(t, func() bool { return len(s.env.Table.List()) == 1 })
	if got := s.must("head -1 < ram:/udp"); got != "datagram\n" {
		t.Fatalf("received %q", got)
	}
	if _, err := s.run("restrict -net dial nc -u -l " + lport); !errors.Is(err, cap.ErrPermission) {
		t.Fatalf("dial-only listen: %v", err)
	}
}

func freeUDPPort(t *testing.T) string {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return strconv.Itoa(c.LocalAddr().(*net.UDPAddr).Port)
}

func TestHTTPD(t *testing.T) {
	s := newShell(t)
	s.env.Net = hostnet.Net{}
	port := freePort(t)
	s.must("echo served by godeep > ram:/page.txt; mkdir ram:/dir")
	s.must("httpd -p " + port + " &")
	get := func(path string) (int, string) {
		var resp *http.Response
		waitFor(t, func() bool {
			var err error
			resp, err = http.Get("http://127.0.0.1:" + port + path)
			return err == nil
		})
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/ram/page.txt"); code != 200 || body != "served by godeep\n" {
		t.Fatalf("file: %d %q", code, body)
	}
	if code, body := get("/"); code != 200 || !strings.Contains(body, "ram/") || !strings.Contains(body, "proc/") {
		t.Fatalf("root: %d %q", code, body)
	}
	if code, body := get("/ram/"); code != 200 || !strings.Contains(body, "page.txt") || !strings.Contains(body, "dir/") {
		t.Fatalf("listing: %d %q", code, body)
	}
	if code, _ := get("/nosuch/x"); code != 404 {
		t.Fatalf("missing volume: %d", code)
	}
	resp, err := http.Post("http://127.0.0.1:"+port+"/ram/x", "text/plain", strings.NewReader("x"))
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	for _, p := range s.env.Table.List() {
		if p.Name == "httpd" {
			p.Kill()
		}
	}
	waitFor(t, func() bool { return len(s.env.Table.List()) == 1 })
	// A restricted view serves only its volumes; dial-only can't serve.
	s.must("restrict -vol ram httpd -p " + port + " &")
	if code, _ := get("/proc/1/status"); code != 404 {
		t.Fatalf("restricted httpd served proc: %d", code)
	}
	if _, err := s.run("restrict -net dial httpd -p " + freePort(t)); !errors.Is(err, cap.ErrPermission) {
		t.Fatalf("dial-only httpd: %v", err)
	}
}
