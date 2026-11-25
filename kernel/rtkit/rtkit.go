// Package rtkit talks to Apple's RTKit coprocessors (SMC, NVMe/ANS, DCP,
// audio, ...) over their mailboxes.
//
// One goroutine (Run) owns the mailbox. It answers the management protocol
// itself: version handshake, endpoint discovery, starting system endpoints,
// power states, and the firmware's requests for shared DMA buffers. Messages
// for application endpoints are routed to per-endpoint channels, so the SMC
// driver, NVMe driver, etc. each just read their own channel.
package rtkit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"godeep/kernel/dma"
)

// Message is one mailbox message.
type Message struct {
	EP   uint8
	Data uint64
}

// Mailbox is the transport: Apple's ASC mailbox on hardware, a fake in tests.
// Send must be safe for concurrent use; Recv delivers inbound messages (the
// mailbox driver feeds it from its interrupt line).
type Mailbox interface {
	Send(ctx context.Context, m Message) error
	Recv() <-chan Message
}

// RTKit is one coprocessor.
type RTKit struct {
	Name string

	mbox  Mailbox
	alloc *dma.Allocator
	iommu dma.IOMMU

	booted chan struct{} // closed when the EPMAP is complete and system EPs started
	pwr    chan int      // IOP power state acks

	mu        sync.Mutex
	version   int
	endpoints map[uint8]bool        // advertised by firmware
	apps      map[uint8]chan uint64 // started application endpoints
	buffers   map[uint8]*dma.Buffer // shared buffers by endpoint
	syslog    struct{ entries, size int }
	err       error
}

// New prepares an RTKit client. Run must be started before Boot.
func New(name string, mbox Mailbox, alloc *dma.Allocator, iommu dma.IOMMU) *RTKit {
	return &RTKit{Name: name, mbox: mbox, alloc: alloc, iommu: iommu,
		booted: make(chan struct{}), pwr: make(chan int, 1),
		endpoints: map[uint8]bool{}, apps: map[uint8]chan uint64{}, buffers: map[uint8]*dma.Buffer{}}
}

// Run is the mailbox owner loop. It returns when ctx is done or the
// coprocessor misbehaves (bad version, crash).
func (r *RTKit) Run(ctx context.Context) error {
	defer r.freeBuffers()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-r.mbox.Recv():
			if !ok {
				return errors.New("rtkit: mailbox closed")
			}
			if err := r.handle(ctx, m); err != nil {
				r.mu.Lock()
				r.err = err
				r.mu.Unlock()
				return err
			}
		}
	}
}

func (r *RTKit) send(ctx context.Context, ep uint8, v uint64) error {
	return r.mbox.Send(ctx, Message{EP: ep, Data: v})
}

func (r *RTKit) handle(ctx context.Context, m Message) error {
	switch {
	case m.EP == EPManagement:
		return r.management(ctx, m.Data)
	case m.EP < EPAppFirst:
		return r.system(ctx, m.EP, m.Data)
	}
	r.mu.Lock()
	ch := r.apps[m.EP]
	r.mu.Unlock()
	if ch == nil {
		return nil // endpoint not started: drop
	}
	select { // backpressure: a slow endpoint driver slows the mailbox
	case ch <- m.Data:
	case <-ctx.Done():
	}
	return nil
}

func (r *RTKit) management(ctx context.Context, v uint64) error {
	switch msgType(v) {
	case mgmtHello:
		lo, hi := helloVersions(v)
		ver := min(hi, MaxVersion)
		if ver < MinVersion || ver < lo {
			return fmt.Errorf("rtkit %s: unsupported firmware versions %d..%d", r.Name, lo, hi)
		}
		r.mu.Lock()
		r.version = ver
		r.mu.Unlock()
		return r.send(ctx, EPManagement, helloReply(ver, ver))

	case mgmtEPMap:
		base, bitmap, last := epmapFields(v)
		r.mu.Lock()
		for i := 0; i < 32; i++ {
			if bitmap&(1<<i) != 0 {
				r.endpoints[uint8(base*32+i)] = true
			}
		}
		r.mu.Unlock()
		if err := r.send(ctx, EPManagement, epmapReply(base, !last)); err != nil {
			return err
		}
		if !last {
			return nil
		}
		select {
		case <-r.booted:
			return nil // duplicate LAST page from firmware: already started
		default:
		}
		// start the system endpoints the firmware advertised
		for _, ep := range []uint8{EPCrashlog, EPSyslog, EPDebug, EPIOReport, EPOSLog, EPTraceKit} {
			if r.has(ep) {
				if err := r.send(ctx, EPManagement, startEP(ep)); err != nil {
					return err
				}
			}
		}
		close(r.booted)
		return nil

	case mgmtIOPPwrStateAck:
		select {
		case r.pwr <- int(field(v, 15, 0)):
		default:
		}
		return nil

	case mgmtSetAPPwrState:
		return nil // firmware acknowledging our AP state: nothing to do
	}
	return nil // unknown management message: ignore
}

func (r *RTKit) system(ctx context.Context, ep uint8, v uint64) error {
	switch {
	case msgType(v) == bufferRequest:
		pages, iova := bufferFields(v)
		if ep == EPCrashlog && r.haveBuffer(ep) {
			return fmt.Errorf("rtkit %s: coprocessor crashed (crashlog at iova %#x)", r.Name, iova)
		}
		if iova != 0 { // firmware supplied its own buffer: just acknowledge
			return r.send(ctx, ep, bufferReply(pages, iova))
		}
		if r.alloc == nil || r.iommu == nil {
			return fmt.Errorf("rtkit %s: endpoint %#x wants a buffer but no DMA allocator", r.Name, ep)
		}
		b, err := r.alloc.Alloc(pages*4096, 16384)
		if err != nil {
			return err
		}
		io, err := r.iommu.Map(b, dma.Bidirectional)
		if err != nil {
			b.Free()
			return err
		}
		r.mu.Lock()
		r.buffers[ep] = b
		r.mu.Unlock()
		return r.send(ctx, ep, bufferReply(pages, io))

	case ep == EPSyslog && msgType(v) == syslogInit:
		r.mu.Lock()
		r.syslog.entries, r.syslog.size = int(field(v, 7, 0)), int(field(v, 31, 24))
		r.mu.Unlock()
		return nil

	case ep == EPSyslog && msgType(v) == syslogLog:
		return r.send(ctx, ep, v) // ack by echoing the index back
	}
	return nil
}

func (r *RTKit) has(ep uint8) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.endpoints[ep]
}

func (r *RTKit) haveBuffer(ep uint8) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buffers[ep] != nil
}

func (r *RTKit) freeBuffers() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ep, b := range r.buffers {
		if r.iommu != nil {
			r.iommu.Unmap(b.Phys, b.Size())
		}
		b.Free()
		delete(r.buffers, ep)
	}
}

// Boot waits for the handshake and endpoint discovery, then powers the
// coprocessor on.
func (r *RTKit) Boot(ctx context.Context) error {
	select {
	case <-r.booted:
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.SetPower(ctx, PwrOn)
}

// SetPower requests an IOP power state and waits for the firmware's ack.
func (r *RTKit) SetPower(ctx context.Context, state int) error {
	if err := r.send(ctx, EPManagement, pwrState(mgmtSetIOPPwrState, state)); err != nil {
		return err
	}
	select {
	case got := <-r.pwr:
		if got != state {
			return fmt.Errorf("rtkit %s: asked for power state %#x, got %#x", r.Name, state, got)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Endpoint starts an application endpoint and returns its channels.
func (r *RTKit) Endpoint(ctx context.Context, ep uint8) (recv <-chan uint64, send func(context.Context, uint64) error, err error) {
	if ep < EPAppFirst {
		return nil, nil, fmt.Errorf("rtkit: endpoint %#x is a system endpoint", ep)
	}
	if !r.has(ep) {
		return nil, nil, fmt.Errorf("rtkit %s: firmware has no endpoint %#x", r.Name, ep)
	}
	ch := make(chan uint64, 32)
	r.mu.Lock()
	r.apps[ep] = ch
	r.mu.Unlock()
	if err := r.send(ctx, EPManagement, startEP(ep)); err != nil {
		return nil, nil, err
	}
	return ch, func(ctx context.Context, v uint64) error { return r.send(ctx, ep, v) }, nil
}

// Info describes the coprocessor for diagnostics.
type Info struct {
	Version   int
	Endpoints []uint8
	Buffers   map[uint8]int // endpoint -> shared buffer size
	Err       error
}

func (r *RTKit) Info() Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	in := Info{Version: r.version, Buffers: map[uint8]int{}, Err: r.err}
	for ep := range r.endpoints {
		in.Endpoints = append(in.Endpoints, ep)
	}
	sort.Slice(in.Endpoints, func(i, j int) bool { return in.Endpoints[i] < in.Endpoints[j] })
	for ep, b := range r.buffers {
		in.Buffers[ep] = b.Size()
	}
	return in
}
