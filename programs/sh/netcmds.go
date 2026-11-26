package sh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	_ "golang.org/x/crypto/x509roots/fallback"

	"godeep/kernel/cap"
	"godeep/kernel/proc"
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
	c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return n.Dial(ctx, "tcp", addr) },
	}}
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
