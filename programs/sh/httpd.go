package sh

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"godeep/kernel/proc"
)

func init() {
	builtins["httpd"] = builtin{"httpd [-p PORT]: serve this shell's volumes read-only over HTTP (/VOLUME/PATH; port 80) until ^C or kill", httpd}
}

// httpd serves the volumes the shell can see, read-only: GET /ram/x is
// ram:/x, a directory gets a listing, / lists the volumes. It's an
// ordinary server on cap.Net, so restrict -vol narrows what it shows and
// restrict -net dial refuses it.
func httpd(ctx context.Context, env Env, p *proc.Proc, args []string) error {
	port := "80"
	switch {
	case len(args) == 2 && args[0] == "-p":
		if n, err := strconv.Atoi(args[1]); err != nil || n < 1 || n > 65535 {
			return errors.New("httpd: bad port")
		}
		port = args[1]
	case len(args) != 0:
		return errors.New("usage: httpd [-p PORT]")
	}
	if env.FS == nil {
		return errors.New("no filesystems")
	}
	n, err := network(ctx, env)
	if err != nil {
		return err
	}
	l, err := n.Listen(ctx, "tcp", net.JoinHostPort("", port))
	if err != nil {
		return err
	}
	var served atomic.Int64
	srv := &http.Server{
		Handler:           volumeHandler(ctx, env, &served),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	fmt.Fprintf(env.out(), "httpd: serving %d volumes on port %s\n", len(env.FS.Names(ctx)), port)
	done := make(chan error, 1)
	if !p.Go(func(context.Context) { done <- srv.Serve(l) }) {
		l.Close()
		return proc.ErrKilled
	}
	var intr <-chan struct{}
	if env.TTY != nil {
		intr = env.TTY.Interrupts()
	}
	select {
	case err := <-done:
		return err
	case <-intr:
	case <-ctx.Done():
	}
	srv.Close()
	<-done
	fmt.Fprintf(env.out(), "httpd: stopped after %d requests\n", served.Load())
	return nil
}

func volumeHandler(ctx context.Context, env Env, served *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/")
		name, _, _ := strings.Cut(rest, "/")
		if name == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintln(w, "<pre>")
			for _, v := range env.FS.Names(ctx) {
				fmt.Fprintf(w, "<a href=\"/%s/\">%s/</a>\n", html.EscapeString(v), html.EscapeString(v))
			}
			fmt.Fprintln(w, "</pre>")
			return
		}
		vol, err := env.FS.Open(r.Context(), name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		http.StripPrefix("/"+name, http.FileServerFS(vol)).ServeHTTP(w, r)
	})
}
