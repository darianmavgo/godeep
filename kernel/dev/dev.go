// Package dev is the device manager. A board describes its hardware as Nodes
// (later parsed from a device tree or Apple's ADT); drivers register by
// `compatible` string. Each device's driver runs as its own process, named
// "drv/<device>", supervised by the "devmgr" process:
//
//   - start order: a driver starts only after every device in Node.Needs is
//     ready (dependencies are waited on, not hand-sequenced)
//   - readiness: a driver calls Device.Ready(service) once usable; consumers
//     block in Manager.Get until then
//   - restart: if a driver fails (returns an error or panics in any of its
//     goroutines), devmgr restarts it with backoff, up to MaxRestarts
package dev

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"godeep/kernel/dma"
	"godeep/kernel/irq"
	"godeep/kernel/proc"
)

// Range is a register window.
type Range struct{ Base, Size uint64 }

// Node describes one device.
type Node struct {
	Name       string   // unique, e.g. "uart0", "rtc"
	Compatible string   // driver match, e.g. "arm,pl031"
	Reg        []Range  // MMIO windows
	IRQ        []int    // interrupt IDs at the board's controller
	Needs      []string // devices that must be ready first
	// Config holds driver-specific settings the board resolved (e.g. from
	// the device tree), or an error saying why it couldn't; nil if none.
	Config any
}

// Driver handles every Node with a matching Compatible.
type Driver struct {
	Compatible string
	// Run initializes the device, calls d.Ready(service) once usable, and
	// keeps servicing it until ctx is done. Return nil when done (e.g. a
	// probe-only driver); an error or panic triggers a restart.
	Run func(ctx context.Context, d *Device) error
}

var drivers = map[string]Driver{}

// Register adds a driver. Call only from init().
func Register(d Driver) { drivers[d.Compatible] = d }

// HasDriver reports whether a driver is registered for compatible.
func HasDriver(compatible string) bool {
	_, ok := drivers[compatible]
	return ok
}

// Device is what a driver instance is given.
type Device struct {
	Node
	Proc   *proc.Proc
	Bridge *irq.Bridge    // interrupt bridge; nil if the board has none
	DMA    *dma.Allocator // nil if the board has no DMA region
	m      *Manager
	e      *entry
}

// IRQ0 returns the device's first interrupt ID (-1 if none).
func (d *Device) IRQ0() int {
	if len(d.IRQ) == 0 {
		return -1
	}
	return d.IRQ[0]
}

// Ready publishes the driver's service object; consumers' Get returns it.
func (d *Device) Ready(service any) { d.e.set(Ready, service, nil) }

// Lookup waits for another device and returns its service.
func (d *Device) Lookup(ctx context.Context, name string) (any, error) {
	return d.m.Get(ctx, name)
}

// State of a device.
type State string

const (
	NoDriver   State = "no driver"
	Waiting    State = "waiting"  // for dependencies
	Starting   State = "starting" // driver running, not ready yet
	Ready      State = "ready"
	Restarting State = "restarting" // crashed, backing off
	Failed     State = "failed"
	Stopped    State = "stopped"
)

type entry struct {
	node Node

	mu       sync.Mutex
	state    State
	svc      any
	err      error
	pid      int
	restarts int
	changed  chan struct{} // closed and replaced on every change
}

func (e *entry) set(s State, svc any, err error) {
	e.mu.Lock()
	e.state, e.svc, e.err = s, svc, err
	close(e.changed)
	e.changed = make(chan struct{})
	e.mu.Unlock()
}

// Manager supervises drivers.
type Manager struct {
	MaxRestarts int
	Backoff     time.Duration

	tab *proc.Table
	irq *irq.Bridge
	dma *dma.Allocator

	mu      sync.Mutex
	entries map[string]*entry
}

func NewManager(tab *proc.Table, b *irq.Bridge, a *dma.Allocator) *Manager {
	return &Manager{MaxRestarts: 3, Backoff: 100 * time.Millisecond,
		tab: tab, irq: b, dma: a, entries: map[string]*entry{}}
}

// ErrNoDevice is returned by Get for an unknown device.
var ErrNoDevice = errors.New("no such device")

// Start spawns the devmgr process, which brings up every node.
func (m *Manager) Start(ctx context.Context, nodes []Node) *proc.Proc {
	m.mu.Lock()
	for _, n := range nodes {
		st := Waiting
		if _, ok := drivers[n.Compatible]; !ok {
			st = NoDriver
		}
		m.entries[n.Name] = &entry{node: n, state: st, changed: make(chan struct{})}
	}
	m.mu.Unlock()

	return m.tab.Spawn(ctx, 0, proc.Spec{Name: "devmgr", Main: func(ctx context.Context, p *proc.Proc) error {
		for _, n := range nodes {
			if drv, ok := drivers[n.Compatible]; ok {
				e := m.entry(n.Name)
				p.Go(func(ctx context.Context) { m.supervise(ctx, p, drv, e) })
			}
		}
		<-ctx.Done()
		return nil
	}})
}

func (m *Manager) entry(name string) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[name]
}

func (m *Manager) supervise(ctx context.Context, mgr *proc.Proc, drv Driver, e *entry) {
	for _, dep := range e.node.Needs {
		if _, err := m.Get(ctx, dep); err != nil {
			if ctx.Err() == nil {
				e.set(Failed, nil, fmt.Errorf("dependency %s: %w", dep, err))
			}
			return
		}
	}
	for {
		e.set(Starting, nil, nil)
		d := &Device{Node: e.node, Bridge: m.irq, DMA: m.dma, m: m, e: e}
		p := m.tab.Spawn(ctx, mgr.PID, proc.Spec{Name: "drv/" + e.node.Name, Main: func(ctx context.Context, p *proc.Proc) error {
			d.Proc = p
			return drv.Run(ctx, d)
		}})
		e.mu.Lock()
		e.pid = p.PID
		e.mu.Unlock()

		<-p.Done()
		err := p.Err()
		e.mu.Lock()
		e.pid = 0
		st, svc := e.state, e.svc
		e.mu.Unlock()

		switch {
		case ctx.Err() != nil || errors.Is(err, proc.ErrKilled):
			e.set(Stopped, nil, err)
			return
		case err == nil && st == Ready:
			e.set(Ready, svc, nil) // finished its work, service stays usable
			return
		case err == nil:
			e.set(Stopped, nil, nil)
			return
		}

		e.mu.Lock()
		e.restarts++
		n := e.restarts
		e.mu.Unlock()
		if n > m.MaxRestarts {
			e.set(Failed, nil, err)
			return
		}
		e.set(Restarting, nil, err)
		select {
		case <-time.After(m.Backoff << (n - 1)):
		case <-ctx.Done():
			e.set(Stopped, nil, err)
			return
		}
	}
}

// Get waits until device name is ready and returns its service. It fails
// fast if the device has no driver or has failed.
func (m *Manager) Get(ctx context.Context, name string) (any, error) {
	e := m.entry(name)
	if e == nil {
		return nil, fmt.Errorf("%s: %w", name, ErrNoDevice)
	}
	for {
		e.mu.Lock()
		st, svc, err, ch := e.state, e.svc, e.err, e.changed
		e.mu.Unlock()
		switch st {
		case Ready:
			return svc, nil
		case NoDriver:
			return nil, fmt.Errorf("%s: no driver for %q", name, e.node.Compatible)
		case Failed:
			return nil, fmt.Errorf("%s: failed: %v", name, err)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Status is a snapshot of one device for `lsdev`.
type Status struct {
	Name, Compatible string
	State            State
	PID              int
	Restarts         int
	IRQ              []int
	Reg              []Range
	Err              error
	Service          any
}

func (m *Manager) List() []Status {
	m.mu.Lock()
	es := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		es = append(es, e)
	}
	m.mu.Unlock()
	out := make([]Status, 0, len(es))
	for _, e := range es {
		e.mu.Lock()
		out = append(out, Status{Name: e.node.Name, Compatible: e.node.Compatible, State: e.state,
			PID: e.pid, Restarts: e.restarts, IRQ: e.node.IRQ, Reg: e.node.Reg, Err: e.err, Service: e.svc})
		e.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Services exposed by common drivers.

// Clock is a wall-clock source (e.g. an RTC).
type Clock interface{ Now() time.Time }

// LatencyProbe measures interrupt-to-goroutine latency.
type LatencyProbe interface {
	Measure(ctx context.Context, n int) ([]time.Duration, error)
}

// Lister reports discovered sub-devices (e.g. virtio-mmio slots).
type Lister interface{ Devices() []string }

// Rebooter resets the machine (e.g. a watchdog with a zero timeout). Reboot
// may return before the reset lands.
type Rebooter interface{ Reboot() }
