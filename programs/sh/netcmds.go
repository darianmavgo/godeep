package sh

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	_ "golang.org/x/crypto/x509roots/fallback"

	"godeep/fs/ninep"
	"godeep/kernel/cap"
	"godeep/kernel/proc"
	"godeep/kernel/vfs"
)

// network returns the shell's network once it's up.
func network(ctx context.Context, env Env) (cap.Net, error) {
	if env.Net == nil {
		return nil, errors.New("no network on this board")
	}
	if p, ok := env.Net.(*cap.PendingNet); ok {
		return p.Wait(ctx)
	}
	return env.Net, nil
}

// host NAME: resolve a name (DNS).
func hostCmd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: host NAME")
	}
	n, err := network(ctx, env)
	if err != nil {
		return err
	}
	r, ok := n.(cap.Resolver)
	if !ok {
		return errors.New("this network doesn't resolve names")
	}
	addrs, err := r.LookupHost(ctx, args[0])
	if err != nil {
		return err
	}
	for _, a := range addrs {
		fmt.Fprintf(env.TTY, "%s has address %s\n", args[0], a)
	}
	return nil
}

// get URL: an HTTP(S) GET, the body to the terminal. TLS servers are
// checked against Mozilla's root certificates, built into the image
// (x509roots/fallback: Go uses them when the system has none, as on TamaGo).
func getCmd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: get http[s]://HOST[:PORT]/PATH")
	}
	if !strings.HasPrefix(args[0], "http://") && !strings.HasPrefix(args[0], "https://") {
		return errors.New("usage: get http[s]://HOST[:PORT]/PATH")
	}
	n, err := network(ctx, env)
	if err != nil {
		return err
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return n.Dial(ctx, "tcp", addr) },
	}
	if clk := rtc(env); clk != nil { // certificates are checked against the real date
		tr.TLSClientConfig = &tls.Config{Time: clk.Now}
	}
	c := &http.Client{Timeout: 30 * time.Second, Transport: tr}
	req, err := http.NewRequestWithContext(ctx, "GET", args[0], nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(env.TTY, "%s\n", resp.Status)
	}
	_, err = io.Copy(env.TTY, ctxReader{ctx, resp.Body})
	return err
}

// mount9p HOST:PORT NAME: mount a 9P2000.L TCP server (e.g. tools/ninepd)
// as volume NAME in the shell's namespace.
func mount9pCmd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: mount9p HOST:PORT NAME (e.g. mount9p 10.0.2.2:5640 dev)")
	}
	if env.FS == nil {
		return errors.New("no filesystems")
	}
	addr, name := args[0], args[1]
	if name == "" || name == "ram" || strings.ContainsAny(name, ":/ \t") {
		return fmt.Errorf("invalid volume name %q", name)
	}
	n, err := network(ctx, env)
	if err != nil {
		return err
	}
	tr, err := ninep.DialTCP(ctx, n.Dial, addr, 64<<10)
	if err != nil {
		return err
	}
	// Cancel a stalled handshake on ^C/kill without cancelling the mounted
	// client after mount9p returns.
	stop := context.AfterFunc(ctx, func() { tr.Close() })
	c, err := ninep.Mount(env.FS.Context(), tr, addr)
	stop()
	if err != nil {
		tr.Close()
		return err
	}
	env.FS.Mount(name, c)
	mode := "ro"
	if vfs.IsWritable(c) {
		mode = "rw"
	}
	fmt.Fprintf(env.TTY, "mounted %s as %s: (%s)\n", addr, name, mode)
	return nil
}
