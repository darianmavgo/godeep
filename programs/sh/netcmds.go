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

// get URL: an HTTP GET, the body to the terminal (http only: TamaGo has
// no root certificates to check TLS servers with).
func getCmd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: get http://HOST[:PORT]/PATH")
	}
	if !strings.HasPrefix(args[0], "http://") {
		return errors.New("http:// only (no TLS root certificates yet)")
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
