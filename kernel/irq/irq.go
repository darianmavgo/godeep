// Package irq is the interrupt bridge: it turns hardware interrupts into
// channel events for driver goroutines.
//
// Interrupt handlers can't block, allocate or run driver code, so the board
// does the minimum in its service routine: acknowledge each pending
// interrupt and call Dispatch. Dispatch masks the line at the controller and
// signals the driver's channel without blocking. The driver services its
// device (clearing the cause), then calls Line.Done to unmask. This is the
// "oneshot" pattern, and it keeps level-triggered lines from re-firing before
// their driver has run.
package irq

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Controller is an interrupt controller (ARM GICv3 on QEMU virt, Apple AIC2
// on M1).
type Controller interface {
	Enable(id int)
	Disable(id int)
}

// Line is one interrupt line owned by one driver.
type Line struct {
	ID   int
	Name string
	// C receives when the interrupt fired. It is buffered (1), so several
	// interrupts before the driver runs coalesce into one event.
	C <-chan struct{}

	c       chan struct{}
	b       *Bridge
	count   atomic.Uint64
	pending atomic.Int64 // dispatch time (ns since bridge start) awaiting Done
	latSum  atomic.Int64 // total dispatch→Done time, ns
	latMax  atomic.Int64
}

// Done re-enables the line after the driver has serviced the device.
func (l *Line) Done() {
	if t := l.pending.Swap(0); t != 0 {
		d := l.b.since() - t
		l.latSum.Add(d)
		for {
			m := l.latMax.Load()
			if d <= m || l.latMax.CompareAndSwap(m, d) {
				break
			}
		}
	}
	l.b.ctl.Enable(l.ID)
}

// Release disables the line and frees it for another driver.
func (l *Line) Release() {
	l.b.ctl.Disable(l.ID)
	l.b.mu.Lock()
	delete(l.b.lines, l.ID)
	l.b.mu.Unlock()
}

// Bridge routes interrupts to Lines.
type Bridge struct {
	ctl   Controller
	start time.Time

	mu       sync.RWMutex
	lines    map[int]*Line
	spurious atomic.Uint64
}

func NewBridge(ctl Controller) *Bridge {
	return &Bridge{ctl: ctl, start: time.Now(), lines: map[int]*Line{}}
}

func (b *Bridge) since() int64 { return int64(time.Since(b.start)) + 1 }

// Request claims interrupt id for a driver and enables it.
func (b *Bridge) Request(id int, name string) (*Line, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if l, ok := b.lines[id]; ok {
		return nil, fmt.Errorf("irq %d already owned by %s", id, l.Name)
	}
	c := make(chan struct{}, 1)
	l := &Line{ID: id, Name: name, C: c, c: c, b: b}
	b.lines[id] = l
	b.ctl.Enable(id)
	return l, nil
}

// Dispatch delivers interrupt id. The board calls it from its interrupt
// service goroutine for every acknowledged interrupt. It never blocks.
func (b *Bridge) Dispatch(id int) {
	b.mu.RLock()
	l := b.lines[id]
	b.mu.RUnlock()
	if l == nil {
		b.spurious.Add(1)
		b.ctl.Disable(id) // nobody owns it: keep it quiet
		return
	}
	b.ctl.Disable(id)
	l.count.Add(1)
	l.pending.CompareAndSwap(0, b.since())
	select {
	case l.c <- struct{}{}:
	default: // already signalled; coalesce
	}
}

// LineStat is a snapshot of one line for `irqstat`.
type LineStat struct {
	ID      int
	Name    string
	Count   uint64
	AvgDone time.Duration // average time from dispatch to the driver's Done
	MaxDone time.Duration
}

// Stats returns per-line statistics and the count of unowned interrupts.
func (b *Bridge) Stats() (lines []LineStat, spurious uint64) {
	b.mu.RLock()
	for _, l := range b.lines {
		s := LineStat{ID: l.ID, Name: l.Name, Count: l.count.Load(), MaxDone: time.Duration(l.latMax.Load())}
		if s.Count > 0 {
			s.AvgDone = time.Duration(l.latSum.Load() / int64(s.Count))
		}
		lines = append(lines, s)
	}
	b.mu.RUnlock()
	sort.Slice(lines, func(i, j int) bool { return lines[i].ID < lines[j].ID })
	return lines, b.spurious.Load()
}
