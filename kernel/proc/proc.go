// Package proc implements godeep processes: a Proc is a supervised group of
// goroutines sharing a context, a PID and pprof labels.
//
// Every goroutine of a Proc carries the labels pid=<PID> and proc=<name>.
// Go propagates labels to child goroutines automatically, so even goroutines
// started with a bare `go` are attributed to their Proc in Snapshot().
package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/pprof"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Main is a program entry point. It must return when ctx is cancelled.
type Main func(ctx context.Context, p *Proc) error

// ErrKilled is the exit error of a Proc stopped by Kill.
var ErrKilled = errors.New("killed")

// Spec describes a Proc to spawn.
type Spec struct {
	Name   string
	Args   []string
	Stdout io.Writer
	Main   Main
}

// Proc is a process: a set of goroutines that live and die together.
type Proc struct {
	PID, PPID int
	Name      string
	Args      []string
	Stdout    io.Writer
	Started   time.Time

	table  *Table
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}

	mu   sync.Mutex
	live int // tracked goroutines still running (main + p.Go)
	err  error
}

// Context returns the Proc's root context (cancelled on kill or main return).
func (p *Proc) Context() context.Context { return p.ctx }

// Done is closed once every tracked goroutine of the Proc has returned.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Err is the exit error: main's return value, ErrKilled, or a panic.
func (p *Proc) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// State is "running", "exiting" (cancelled, goroutines still unwinding) or "dead".
func (p *Proc) State() string {
	select {
	case <-p.done:
		return "dead"
	default:
	}
	if p.ctx.Err() != nil {
		return "exiting"
	}
	return "running"
}

// Kill cancels the Proc. Kill is cooperative: goroutines stop at their next
// ctx check / blocking kernel call.
func (p *Proc) Kill() {
	p.setErr(ErrKilled)
	p.cancel(ErrKilled)
}

// Wait blocks until the Proc is dead or ctx is done.
func (p *Proc) Wait(ctx context.Context) error {
	select {
	case <-p.done:
		return p.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Go starts an accounted goroutine in the Proc. A panic in fn kills the Proc,
// not the kernel. It returns false if the Proc is already dead.
func (p *Proc) Go(fn func(ctx context.Context)) bool {
	p.mu.Lock()
	if p.live == 0 {
		p.mu.Unlock()
		return false
	}
	p.live++
	p.mu.Unlock()
	p.spawn(func() error { fn(p.ctx); return nil }, false)
	return true
}

// spawn starts a goroutine that carries the Proc's labels from its first
// instruction.
func (p *Proc) spawn(fn func() error, isMain bool) {
	goWithLabels(p.ctx, func() { p.run(fn, isMain) })
}

// goWithLabels starts fn in a goroutine labelled with ctx's pprof labels
// from birth (goroutines inherit labels when created), without touching the
// caller's labels. A short-lived launcher goroutine takes the labels and
// starts fn. (pprof.Do is not usable here: afterwards it sets the caller's
// labels to ctx's, not back to what they were.)
func goWithLabels(ctx context.Context, fn func()) {
	go func() {
		pprof.SetGoroutineLabels(ctx)
		go fn()
	}()
}

func (p *Proc) run(fn func() error, isMain bool) {
	defer p.release()
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("panic: %v", r)
			if p.Stdout != nil {
				fmt.Fprintf(p.Stdout, "[pid %d %s] %v\n", p.PID, p.Name, err)
			}
			p.setErr(err)
			p.cancel(err)
		}
	}()
	err := fn()
	if isMain {
		// Like Go's main: when main returns, the process ends.
		p.setErr(err)
		p.cancel(nil)
	}
}

func (p *Proc) setErr(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
}

func (p *Proc) release() {
	p.mu.Lock()
	p.live--
	last := p.live == 0
	p.mu.Unlock()
	if last {
		p.cancel(nil)
		p.table.remove(p.PID)
		close(p.done)
	}
}

// Table is the process table.
type Table struct {
	mu    sync.Mutex
	next  int
	procs map[int]*Proc
}

func NewTable() *Table { return &Table{next: 1, procs: map[int]*Proc{}} }

// Spawn starts a new Proc whose lifetime is bounded by parent.
func (t *Table) Spawn(parent context.Context, ppid int, s Spec) *Proc {
	ctx, cancel := context.WithCancelCause(parent)
	t.mu.Lock()
	pid := t.next
	t.next++
	p := &Proc{
		PID: pid, PPID: ppid, Name: s.Name, Args: s.Args, Stdout: s.Stdout,
		Started: time.Now(), table: t, cancel: cancel,
		done: make(chan struct{}), live: 1,
	}
	t.procs[pid] = p
	t.mu.Unlock()

	p.ctx = pprof.WithLabels(ctx, pprof.Labels("pid", strconv.Itoa(pid), "proc", s.Name))
	p.spawn(func() error { return s.Main(p.ctx, p) }, true)
	return p
}

func (t *Table) remove(pid int) {
	t.mu.Lock()
	delete(t.procs, pid)
	t.mu.Unlock()
}

// Get returns a live Proc by PID.
func (t *Table) Get(pid int) (*Proc, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.procs[pid]
	return p, ok
}

// Kill kills a Proc by PID.
func (t *Table) Kill(pid int) error {
	p, ok := t.Get(pid)
	if !ok {
		return fmt.Errorf("no such process: %d", pid)
	}
	p.Kill()
	return nil
}

// List returns live Procs ordered by PID.
func (t *Table) List() []*Proc {
	t.mu.Lock()
	out := make([]*Proc, 0, len(t.procs))
	for _, p := range t.procs {
		out = append(out, p)
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// Kthread starts a kernel goroutine (pid 0), e.g. a device driver.
func Kthread(name string, fn func()) {
	goWithLabels(pprof.WithLabels(context.Background(), pprof.Labels("pid", "0", "proc", name)), fn)
}

// Shutdown kills every process and waits until all have exited or ctx is
// done. It returns the processes that did not exit in time (their goroutines
// ignore cancellation; kill is cooperative).
func (t *Table) Shutdown(ctx context.Context) []*Proc {
	for _, p := range t.List() {
		p.Kill()
	}
	for {
		ps := t.List()
		if len(ps) == 0 {
			return nil
		}
		select {
		case <-ps[0].Done():
		case <-ctx.Done():
			return t.List()
		}
	}
}
