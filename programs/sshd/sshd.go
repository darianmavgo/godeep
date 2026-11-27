// Package sshd is godeep's SSH server (golang.org/x/crypto/ssh). Each SSH
// session runs the ordinary godeep shell: the session channel becomes a
// tty.TTY (tty.NewFor), and a child "sh" Proc reads from it, exactly like the
// console's shell. "exec" requests (ssh godeep ps) run "sh -c LINE".
//
// Authority: sshd gets a cap.Net to listen on and a sh.Env template; it
// hands each session the same Env with the session's TTY. Authentication is
// public-key only, against the keys built into the image (keys/, make keys).
//
// Kill: closing the listener and every connection when sshd's ctx ends
// makes all its blocking calls return; sessions' shells are child Procs, so
// they die with it. A client that disconnects kills its session's programs.
//
// Library goroutines: x/crypto/ssh starts its own goroutines with plain `go`
// (connection mux, key exchange). They inherit sshd's pprof labels, so ps
// attributes them to sshd, but a panic in one of them is not contained by
// the Proc and stops the kernel. Host tests fuzz the pre-auth input.
package sshd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"godeep/kernel/cap"
	"godeep/kernel/proc"
	"godeep/kernel/tty"
	"godeep/programs/sh"
)

// Config is what sshd is given.
type Config struct {
	Net   cap.Net
	Addr  string // default ":22"
	Shell sh.Env // template for sessions; TTY is replaced per session
	// HostKey and Authorized default to the keys built into the image.
	HostKey    ssh.Signer
	Authorized []ssh.PublicKey
	// Version is shown in the login banner.
	Version string
	// Log receives one line per connection and error (the console).
	Log io.Writer
}

// HandshakeTimeout bounds key exchange and authentication.
const HandshakeTimeout = 30 * time.Second

// Main returns the sshd program.
func Main(cfg Config) proc.Main {
	return func(ctx context.Context, p *proc.Proc) error {
		if cfg.Addr == "" {
			cfg.Addr = ":22"
		}
		if cfg.Log == nil {
			cfg.Log = io.Discard
		}
		if cfg.HostKey == nil || cfg.Authorized == nil {
			host, auth, err := BuiltinKeys()
			if err != nil {
				return err
			}
			if cfg.HostKey == nil {
				cfg.HostKey = host
			}
			if cfg.Authorized == nil {
				cfg.Authorized = auth
			}
		}
		if len(cfg.Authorized) == 0 {
			return errors.New("no authorized keys built in (run make keys)")
		}
		if cfg.Net == nil {
			return cap.ErrNoNetwork
		}
		l, err := cfg.Net.Listen(ctx, "tcp", cfg.Addr)
		if err != nil {
			return err
		}
		defer l.Close()
		fmt.Fprintf(cfg.Log, "sshd: listening on %s, host key %s\n", cfg.Addr, ssh.FingerprintSHA256(cfg.HostKey.PublicKey()))
		return Serve(ctx, p, l, &cfg)
	}
}

// Serve accepts connections on l until ctx is done.
func Serve(ctx context.Context, p *proc.Proc, l net.Listener, cfg *Config) error {
	sc := serverConfig(cfg)
	stop := context.AfterFunc(ctx, func() { l.Close() }) // kill: Accept returns
	defer stop()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !p.Go(func(ctx context.Context) { serveConn(ctx, p, c, sc, cfg) }) {
			c.Close()
			return nil
		}
	}
}

func serverConfig(cfg *Config) *ssh.ServerConfig {
	sc := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-godeep",
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			m := key.Marshal()
			for _, k := range cfg.Authorized {
				if bytes.Equal(k.Marshal(), m) {
					return &ssh.Permissions{Extensions: map[string]string{"key": ssh.FingerprintSHA256(key)}}, nil
				}
			}
			return nil, errors.New("unknown key")
		},
		BannerCallback: func(ssh.ConnMetadata) string {
			return fmt.Sprintf("godeep %s: development SSH (no real entropy on every board yet)\r\n", cfg.Version)
		},
	}
	sc.AddHostKey(cfg.HostKey)
	return sc
}

func serveConn(ctx context.Context, p *proc.Proc, c net.Conn, sc *ssh.ServerConfig, cfg *Config) {
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()

	c.SetDeadline(time.Now().Add(HandshakeTimeout))
	conn, chans, reqs, err := ssh.NewServerConn(c, sc)
	if err != nil {
		fmt.Fprintf(cfg.Log, "sshd: %s: %v\n", c.RemoteAddr(), err)
		return
	}
	c.SetDeadline(time.Time{})
	defer conn.Close()
	fmt.Fprintf(cfg.Log, "sshd: %s@%s logged in (%s)\n", conn.User(), c.RemoteAddr(), conn.Permissions.Extensions["key"])

	gone := make(chan struct{}) // closed when the client disconnects
	p.Go(func(context.Context) { conn.Wait(); close(gone) })
	p.Go(func(context.Context) { ssh.DiscardRequests(reqs) })
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		p.Go(func(ctx context.Context) { session(ctx, p, ch, creqs, gone, cfg) })
	}
}

// session waits for "shell" or "exec", then runs sh on the channel.
func session(ctx context.Context, p *proc.Proc, ch ssh.Channel, reqs <-chan *ssh.Request, gone <-chan struct{}, cfg *Config) {
	defer ch.Close()
	pty, cols, rows := false, 0, 0
	var args []string
	for args == nil {
		var r *ssh.Request
		select {
		case r = <-reqs:
		case <-ctx.Done():
			return
		}
		if r == nil {
			return // channel closed before a command
		}
		switch r.Type {
		case "pty-req":
			pty = true
			var req struct {
				Term       string
				Cols, Rows uint32
				W, H       uint32
				Modes      string
			}
			if ssh.Unmarshal(r.Payload, &req) == nil {
				cols, rows = int(req.Cols), int(req.Rows)
			}
			r.Reply(true, nil)
		case "env":
			r.Reply(true, nil) // accepted and ignored
		case "shell":
			args = []string{}
			r.Reply(true, nil)
		case "exec":
			var cmd struct{ Command string }
			if ssh.Unmarshal(r.Payload, &cmd) != nil {
				r.Reply(false, nil)
				continue
			}
			args = []string{"-c", cmd.Command}
			r.Reply(true, nil)
		case "subsystem":
			var sub struct{ Name string }
			if ssh.Unmarshal(r.Payload, &sub) != nil || sub.Name != "sftp" || cfg.Shell.FS == nil {
				r.Reply(false, nil)
				continue
			}
			r.Reply(true, nil)
			p.Go(func(context.Context) { ssh.DiscardRequests(reqs) })
			err := ServeSFTP(ctx, ch, cfg.Shell.FS)
			code := uint32(0)
			if err != nil {
				code = 1
			}
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
			return
		default:
			r.Reply(false, nil)
		}
	}

	var out io.Writer = ch
	if pty {
		out = crlf{ch} // the client's terminal is raw
	}
	in := make(chan byte, 256)
	stdinEOF := make(chan struct{})
	t := tty.NewFor(p, in, out, pty)
	defer t.Close()
	t.SetSize(cols, rows) // edit's screen
	p.Go(func(context.Context) { // window-change etc. after start
		for r := range reqs {
			if r.Type == "window-change" {
				var wc struct{ Cols, Rows, W, H uint32 }
				if ssh.Unmarshal(r.Payload, &wc) == nil {
					t.SetSize(int(wc.Cols), int(wc.Rows))
				}
			}
			r.Reply(r.Type == "window-change" || r.Type == "env", nil)
		}
	})
	p.Go(func(ctx context.Context) { pump(ctx, ch, in, stdinEOF) })

	env := cfg.Shell
	env.TTY = t
	// A session gets a narrower network than the kernel's: it may dial
	// (get, host, mount9p) but not listen, so a remote login can't open
	// new services on the machine.
	env.Net = cap.DialOnly(env.Net)
	child := env.Table.Spawn(ctx, p.PID, proc.Spec{Name: "sh", Args: args, Stdout: t, Main: sh.Main(env)})
	interactive := len(args) == 0
	var eof <-chan struct{}
	if interactive {
		eof = stdinEOF // an interactive session ends with its input
	}
	select {
	case <-child.Done():
	case <-gone:
		child.Kill()
	case <-eof:
		child.Kill()
	case <-ctx.Done():
		child.Kill()
	}
	<-child.Done()
	status := uint32(0)
	if err := child.Err(); err != nil && !errors.Is(err, proc.ErrKilled) {
		status = 1
	}
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	ch.CloseWrite()
}

// pump copies the channel's input into the tty's byte channel.
func pump(ctx context.Context, r io.Reader, in chan<- byte, eof chan<- struct{}) {
	defer close(eof)
	defer close(in)
	buf := make([]byte, 256)
	for {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			select {
			case in <- b:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// crlf turns "\n" into "\r\n" for a raw terminal.
type crlf struct{ w io.Writer }

func (c crlf) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte{'\n'}) {
		return c.w.Write(p)
	}
	s := strings.ReplaceAll(strings.ReplaceAll(string(p), "\r\n", "\n"), "\n", "\r\n")
	if _, err := io.WriteString(c.w, s); err != nil {
		return 0, err
	}
	return len(p), nil
}

// NewHostKey makes an ed25519 host key (for boots without a built-in one).
// Ed25519 signatures don't use the random generator, so a weak one can't
// leak this key through signing; only key generation needs good entropy.
func NewHostKey() (ssh.Signer, error) {
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(k)
}
