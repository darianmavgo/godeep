package ipc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
)

// ErrNoServer is returned by Call when the port's server stopped.
var ErrNoServer = errors.New("port has no server")

// Port is a typed request/reply service: one or more server goroutines
// Serve it, clients Call it. Requests travel over a channel, so the server
// owns its state and needs no locks.
type Port[Req, Resp any] struct {
	name  string
	reqs  chan call[Req, Resp]
	calls atomic.Int64

	mu      sync.Mutex
	serving int
	stop    chan struct{} // closed while nobody serves
}

type call[Req, Resp any] struct {
	ctx   context.Context
	req   Req
	reply chan result[Resp]
}

type result[Resp any] struct {
	resp Resp
	err  error
}

// NewPort returns a port with no server yet (Calls wait for one).
func NewPort[Req, Resp any](name string) *Port[Req, Resp] {
	return &Port[Req, Resp]{name: name, reqs: make(chan call[Req, Resp])}
}

// Name is the port's name.
func (p *Port[Req, Resp]) Name() string { return p.name }

// Calls is how many requests have been served.
func (p *Port[Req, Resp]) Calls() int64 { return p.calls.Load() }

// Servers is how many Serve loops run now.
func (p *Port[Req, Resp]) Servers() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.serving
}

// Type describes the request and reply types ("string → int").
func (p *Port[Req, Resp]) Type() string {
	return fmt.Sprintf("%v → %v", reflect.TypeFor[Req](), reflect.TypeFor[Resp]())
}

// Serve answers requests with h until ctx is done. h runs on the Serve
// goroutine, one request at a time; run several Serves for concurrency.
func (p *Port[Req, Resp]) Serve(ctx context.Context, h func(context.Context, Req) (Resp, error)) error {
	p.mu.Lock()
	p.serving++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.serving--
		p.mu.Unlock()
	}()
	for {
		select {
		case c := <-p.reqs:
			resp, err := h(c.ctx, c.req)
			p.calls.Add(1)
			c.reply <- result[Resp]{resp, err} // buffered: never blocks
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Call sends req and waits for the reply. It returns ctx's error if ctx
// ends first: before a server took the request, or while it works on it
// (the server's reply is then dropped).
func (p *Port[Req, Resp]) Call(ctx context.Context, req Req) (Resp, error) {
	var zero Resp
	c := call[Req, Resp]{ctx: ctx, req: req, reply: make(chan result[Resp], 1)}
	select {
	case p.reqs <- c:
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	select {
	case r := <-c.reply:
		return r.resp, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// Described is what a Namespace lists about a port, whatever its types.
type Described interface {
	Name() string
	Type() string
	Calls() int64
	Servers() int
}

// Namespace is a set of named ports: the authority to find services. It's
// a value handed to Procs (sh.Env.Ports), not a global; a narrowed view
// (Only) hides the rest.
type Namespace struct {
	mu    sync.Mutex
	ports map[string]Described
	allow map[string]bool // nil: everything
	base  *Namespace      // set on views
}

// NewNamespace returns an empty namespace.
func NewNamespace() *Namespace { return &Namespace{ports: map[string]Described{}} }

// ErrExists is returned when publishing a name that is taken.
var ErrExists = errors.New("port name in use")

// ErrPermission is returned by a narrowed view for names outside it.
var ErrPermission = errors.New("not in this namespace")

func (n *Namespace) root() *Namespace {
	for n.base != nil {
		n = n.base
	}
	return n
}

func (n *Namespace) allowed(name string) bool {
	for v := n; v != nil; v = v.base {
		if v.allow != nil && !v.allow[name] {
			return false
		}
	}
	return true
}

// Publish adds a port under its name. The returned func removes it (only
// if it's still that port).
func (n *Namespace) Publish(p Described) (unpublish func(), err error) {
	if !n.allowed(p.Name()) {
		return nil, fmt.Errorf("%s: %w", p.Name(), ErrPermission)
	}
	r := n.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.ports[p.Name()]; ok {
		return nil, fmt.Errorf("%s: %w", p.Name(), ErrExists)
	}
	r.ports[p.Name()] = p
	return func() {
		r.mu.Lock()
		if r.ports[p.Name()] == p {
			delete(r.ports, p.Name())
		}
		r.mu.Unlock()
	}, nil
}

// List returns the visible ports, by name.
func (n *Namespace) List() []Described {
	r := n.root()
	r.mu.Lock()
	out := make([]Described, 0, len(r.ports))
	for name, p := range r.ports {
		if n.allowed(name) {
			out = append(out, p)
		}
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Only returns a view that sees (and may publish) just these names.
func (n *Namespace) Only(names ...string) *Namespace {
	allow := map[string]bool{}
	for _, s := range names {
		allow[s] = true
	}
	return &Namespace{allow: allow, base: n}
}

// Lookup finds a port by name and type.
func Lookup[Req, Resp any](n *Namespace, name string) (*Port[Req, Resp], error) {
	if n == nil {
		return nil, fmt.Errorf("%s: no port namespace", name)
	}
	if !n.allowed(name) {
		return nil, fmt.Errorf("%s: %w", name, ErrPermission)
	}
	r := n.root()
	r.mu.Lock()
	d, ok := r.ports[name]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%s: no such port", name)
	}
	p, ok := d.(*Port[Req, Resp])
	if !ok {
		return nil, fmt.Errorf("%s: port is %s", name, d.Type())
	}
	return p, nil
}
