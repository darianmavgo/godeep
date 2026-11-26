package sshd

import (
	"github.com/google/pprof/profile"
	"godeep/kernel/klog"
	"os"
	"testing/fstest"

	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/pkg/sftp"
	"godeep/fs/memfs"
	"godeep/kernel/vfs"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"godeep/kernel/proc"
	"godeep/kernel/tty"
	_ "godeep/programs/coreutils"
	"godeep/programs/sh"
)

// listener is a loopback TCP listener the tests dial.
type listener struct{ net.Listener }

func newListener(t testing.TB) *listener {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return &listener{l}
}

func (l *listener) dial(t testing.TB) net.Conn {
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type server struct {
	tab    *proc.Table
	sshd   *proc.Proc
	l      *listener
	cancel context.CancelFunc
	client ssh.Signer
	host   ssh.PublicKey
	log    *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func signer(t testing.TB) ssh.Signer {
	_, k, _ := ed25519.GenerateKey(nil)
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func startServer(t testing.TB) *server {
	t.Helper()
	tab := proc.NewTable()
	ctx, cancel := context.WithCancel(context.Background())
	console := tty.New(make(chan byte), io.Discard, false)
	host, client := signer(t), signer(t)
	s := &server{tab: tab, l: newListener(t), cancel: cancel, client: client, host: host.PublicKey(), log: &syncBuf{}}
	cfg := &Config{
		Shell:      sh.Env{Table: tab, TTY: console, Boot: time.Now(), FS: testNS(), Log: testLog},
		HostKey:    host,
		Authorized: []ssh.PublicKey{client.PublicKey()},
		Version:    "test",
		Log:        s.log,
	}
	s.sshd = tab.Spawn(ctx, 0, proc.Spec{Name: "sshd", Main: func(ctx context.Context, p *proc.Proc) error {
		return Serve(ctx, p, s.l, cfg)
	}})
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.sshd.Done():
		case <-time.After(5 * time.Second):
			t.Error("sshd didn't exit")
		}
		console.Close()
	})
	return s
}

func (s *server) connect(t testing.TB, key ssh.Signer) (*ssh.Client, error) {
	c := s.l.dial(t)
	cc, chans, reqs, err := ssh.NewClientConn(c, "godeep", &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.FixedHostKey(s.host),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		c.Close()
		return nil, err
	}
	return ssh.NewClient(cc, chans, reqs), nil
}

func TestExec(t *testing.T) {
	s := startServer(t)
	cl, err := s.connect(t, s.client)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	sess, _ := cl.NewSession()
	out, err := sess.Output("echo hello from godeep")
	if err != nil || string(out) != "hello from godeep\n" {
		t.Fatalf("echo: %q, %v", out, err)
	}

	sess, _ = cl.NewSession()
	out, err = sess.Output("ps")
	if err != nil || !strings.Contains(string(out), "sshd") || !strings.Contains(string(out), "PID") {
		t.Fatalf("ps: %q, %v", out, err)
	}

	sess, _ = cl.NewSession()
	_, err = sess.Output("nosuchcommand")
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 1 {
		t.Fatalf("unknown command: %v", err)
	}
	if !strings.Contains(s.log.String(), "root@") {
		t.Fatalf("no login logged: %q", s.log.String())
	}
}

// readUntil reads r until want appears (or fails after 5 s).
func readUntil(t *testing.T, r io.Reader, want string) string {
	t.Helper()
	var got bytes.Buffer
	buf := make([]byte, 512)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %q, got %q", want, got.String())
		}
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if err != nil && !strings.Contains(got.String(), want) {
			t.Fatalf("waiting for %q: %v (got %q)", want, err, got.String())
		}
	}
	return got.String()
}

func TestInteractiveShell(t *testing.T) {
	s := startServer(t)
	cl, err := s.connect(t, s.client)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	sess, _ := cl.NewSession()
	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	readUntil(t, stdout, "godeep$ ")
	io.WriteString(stdin, "echo hi\r")
	out := readUntil(t, stdout, "godeep$ ")
	if !strings.Contains(out, "echo hi\r\n") || !strings.Contains(out, "hi\r\n") {
		t.Fatalf("echo + CRLF: %q", out)
	}

	// ^C kills a foreground program, and the shell carries on.
	io.WriteString(stdin, "sleep 60\r")
	readUntil(t, stdout, "sleep 60")
	time.Sleep(50 * time.Millisecond)
	io.WriteString(stdin, "\x03")
	readUntil(t, stdout, "godeep$ ")

	io.WriteString(stdin, "exit\r")
	if err := sess.Wait(); err != nil {
		t.Fatalf("exit: %v", err)
	}
}

func procsNamed(tab *proc.Table, name string) int {
	n := 0
	for _, p := range tab.List() {
		if p.Name == name {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDisconnectKillsSession(t *testing.T) {
	s := startServer(t)
	cl, err := s.connect(t, s.client)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := cl.NewSession()
	if err := sess.Start("sleep 600"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sleep never started", func() bool { return procsNamed(s.tab, "sleep") == 1 })
	cl.Close() // the client vanishes
	waitFor(t, "session outlived its client", func() bool {
		return procsNamed(s.tab, "sleep") == 0 && procsNamed(s.tab, "sh") == 0
	})
}

func TestStdinEOFDoesNotKillExec(t *testing.T) {
	s := startServer(t)
	cl, err := s.connect(t, s.client)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	sess, _ := cl.NewSession()
	sess.Stdin = strings.NewReader("") // like ssh -n / </dev/null
	out, err := sess.Output("ps")
	if err != nil || !strings.Contains(string(out), "PID") {
		t.Fatalf("ps with closed stdin: %q, %v", out, err)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	s := startServer(t)
	if cl, err := s.connect(t, signer(t)); err == nil {
		cl.Close()
		t.Fatal("unknown key accepted")
	}
}

func TestKillSshd(t *testing.T) {
	s := startServer(t)
	cl, err := s.connect(t, s.client)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	sess, _ := cl.NewSession()
	if err := sess.Start("sleep 600"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sleep never started", func() bool { return procsNamed(s.tab, "sleep") == 1 })
	s.sshd.Kill()
	select {
	case <-s.sshd.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("sshd ignored kill")
	}
	waitFor(t, "sessions survived sshd", func() bool { return len(s.tab.List()) == 0 })
	if err := sess.Wait(); err == nil {
		t.Fatal("session didn't end")
	}
}

func TestCRLF(t *testing.T) {
	var b bytes.Buffer
	crlf{&b}.Write([]byte("a\nb\r\nc"))
	if b.String() != "a\r\nb\r\nc" {
		t.Fatalf("%q", b.String())
	}
}

func TestParseAuthorizedKeys(t *testing.T) {
	k := signer(t).PublicKey()
	file := "# comment\n\n" + string(ssh.MarshalAuthorizedKey(k))
	keys, err := ParseAuthorizedKeys([]byte(file))
	if err != nil || len(keys) != 1 || !bytes.Equal(keys[0].Marshal(), k.Marshal()) {
		t.Fatalf("%v %v", keys, err)
	}
	if _, err := ParseAuthorizedKeys([]byte("garbage\n")); err == nil {
		t.Fatal("garbage accepted")
	}
}

// FuzzPreAuth feeds arbitrary bytes to the server side of a handshake: no
// input may panic (library goroutines' panics would stop the kernel).
func FuzzPreAuth(f *testing.F) {
	f.Add([]byte("SSH-2.0-OpenSSH_9.9\r\n"))
	f.Add([]byte("SSH-2.0-x\r\n\x00\x00\x00\x0c\x0a\x14\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"))
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"))
	host := signer(f)
	sc := serverConfig(&Config{HostKey: host, Authorized: []ssh.PublicKey{signer(f).PublicKey()}})
	f.Fuzz(func(t *testing.T, data []byte) {
		a, b := net.Pipe()
		go func() {
			b.SetDeadline(time.Now().Add(time.Second))
			b.Write(data)
			io.Copy(io.Discard, b) // drain the server's version line
		}()
		a.SetDeadline(time.Now().Add(time.Second))
		go func() { time.Sleep(200 * time.Millisecond); b.Close() }()
		if c, _, _, err := ssh.NewServerConn(a, sc); err == nil {
			c.Close()
			t.Fatal("handshake succeeded on fuzz input")
		}
		a.Close()
	})
}

// roVol is a read-only volume.
type roVol struct{ fstest.MapFS }

func (roVol) Type() string  { return "test" }
func (roVol) Label() string { return "" }

func testNS() *vfs.NS {
	ns := vfs.New(context.Background(), nil)
	ns.Mount("ram", memfs.New("ram"))
	ns.Mount("disk", roVol{fstest.MapFS{"docs/readme.md": {Data: []byte("read me")}}})
	return ns
}

func TestSFTP(t *testing.T) {
	s := startServer(t)
	cl, err := s.connect(t, s.client)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	c, err := sftp.NewClient(cl)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if wd, err := c.Getwd(); err != nil || wd != "/ram" {
		t.Fatalf("home %q %v", wd, err)
	}
	// Upload (relative path: lands in /ram), several write packets' worth.
	data := bytes.Repeat([]byte("0123456789abcdef"), 20000) // 320 KB
	f, err := c.Create("big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, err := c.Open("/ram/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	back, err := io.ReadAll(got)
	got.Close()
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("read back %d bytes, %v", len(back), err)
	}
	if fi, err := c.Stat("/ram/big.bin"); err != nil || fi.Size() != int64(len(data)) || fi.IsDir() {
		t.Fatalf("stat %v %v", fi, err)
	}

	// The root lists volumes; directories list their files.
	root, err := c.ReadDir("/")
	if err != nil || len(root) != 2 || root[0].Name() != "disk" || !root[1].IsDir() {
		t.Fatalf("/: %v %v", root, err)
	}
	if err := c.Mkdir("/ram/d"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename("/ram/big.bin", "/ram/d/moved.bin"); err != nil {
		t.Fatal(err)
	}
	ents, err := c.ReadDir("/ram/d")
	if err != nil || len(ents) != 1 || ents[0].Name() != "moved.bin" {
		t.Fatalf("/ram/d: %v %v", ents, err)
	}
	if err := c.Truncate("/ram/d/moved.bin", 10); err != nil {
		t.Fatal(err)
	}
	if fi, _ := c.Stat("/ram/d/moved.bin"); fi.Size() != 10 {
		t.Fatalf("truncate: %d", fi.Size())
	}
	if err := c.Remove("/ram/d/moved.bin"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDirectory("/ram/d"); err != nil {
		t.Fatal(err)
	}

	// Read-only volumes: readable, not writable.
	rf, err := c.Open("/disk/docs/readme.md")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rf)
	rf.Close()
	if string(b) != "read me" {
		t.Fatalf("disk read %q", b)
	}
	if _, err := c.Create("/disk/x"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("write to a read-only volume: %v", err)
	}
	if _, err := c.Stat("/nope/x"); err == nil {
		t.Fatal("stat in a missing volume")
	}
	if _, err := c.Open("/ram/missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
}

var testLog = klog.New(nil, 0)

func TestPprofAndDmesgOverExec(t *testing.T) {
	testLog.Printf("netd: hello from the kernel log")
	s := startServer(t)
	cl, err := s.connect(t, s.client)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	sess, _ := cl.NewSession()
	out, err := sess.Output("pprof goroutine")
	if err != nil || len(out) < 100 || out[0] != 0x1f || out[1] != 0x8b {
		t.Fatalf("pprof: %d bytes, %v (want gzip)", len(out), err)
	}
	if _, err := profile.Parse(bytes.NewReader(out)); err != nil {
		t.Fatalf("not a profile: %v", err)
	}
	sess, _ = cl.NewSession()
	out, err = sess.Output("dmesg")
	if err != nil || !strings.Contains(string(out), "] netd: hello from the kernel log") {
		t.Fatalf("dmesg: %q %v", out, err)
	}
}
