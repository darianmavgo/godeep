package agx

import (
	"context"
	"fmt"
	"time"
)

// Channels are ring buffers shared with the firmware (Asahi: channel.rs,
// fw/channels.rs). A ring is an array of fixed-size messages plus a state
// block with a read and a write pointer (message indices). The writer
// fills a slot, then publishes it by advancing the write pointer; the
// reader consumes and advances the read pointer.
//
// State blocks (per sub-channel): ChannelState is read_ptr at 0x0 and
// write_ptr at 0x20 in 0x30 bytes; FwCtlChannelState is read_ptr at 0x0,
// write_ptr at 0x10 in 0x20 bytes.

// StateKind selects a state block layout.
type StateKind struct{ Size, ReadPtr, WritePtr int }

var (
	ChannelState      = StateKind{Size: 0x30, ReadPtr: 0x00, WritePtr: 0x20}
	FwCtlChannelState = StateKind{Size: 0x20, ReadPtr: 0x00, WritePtr: 0x10}
)

// Ring is one channel: its state and its messages, in kernel GPU memory.
type Ring struct {
	State, Msgs *Object
	kind        StateKind
	count       uint32 // messages per sub-channel
	size        int    // bytes per message
	subs        int
}

func newRing(k *KernelAlloc, kind StateKind, subs, count, size int, msgClass Class) (*Ring, error) {
	st, err := k.Alloc(Shared, kind.Size*subs)
	if err != nil {
		return nil, err
	}
	m, err := k.Alloc(msgClass, count*size*subs)
	if err != nil {
		return nil, err
	}
	return &Ring{State: st, Msgs: m, kind: kind, count: uint32(count), size: size, subs: subs}, nil
}

// Raw is the ring as InitData points to it: (state, ring) GPU addresses.
func (r *Ring) Raw() (state, ring uint64) { return r.State.VA, r.Msgs.VA }

func (r *Ring) ptr(sub, off int) int { return sub*r.kind.Size + off }

// RxChannel receives from the firmware.
type RxChannel struct {
	*Ring
	rptr [6]uint32
}

// NewRx allocates a receive channel of count messages of size bytes, on
// subs sub-channels (the firmware log has 6).
func NewRx(k *KernelAlloc, kind StateKind, subs, count, size int) (*RxChannel, error) {
	if subs < 1 || subs > 6 {
		return nil, fmt.Errorf("agx: %d sub-channels", subs)
	}
	r, err := newRing(k, kind, subs, count, size, Shared)
	if err != nil {
		return nil, err
	}
	return &RxChannel{Ring: r}, nil
}

// Get returns the next message on a sub-channel (a copy), if any.
func (c *RxChannel) Get(sub int) ([]byte, bool) {
	w := c.State.Buf.Load32(c.ptr(sub, c.kind.WritePtr))
	r := c.rptr[sub]
	if w == r {
		return nil, false
	}
	if w >= c.count {
		return nil, false // the firmware wrote garbage: don't index with it
	}
	off := (sub*int(c.count) + int(r)) * c.size
	msg := append([]byte(nil), c.Msgs.Bytes()[off:off+c.size]...)
	r = (r + 1) % c.count
	c.rptr[sub] = r
	c.State.Buf.Store32(c.ptr(sub, c.kind.ReadPtr), r)
	return msg, true
}

// TxChannel sends to the firmware.
type TxChannel struct {
	*Ring
	wptr uint32
}

// NewTx allocates a transmit channel.
func NewTx(k *KernelAlloc, kind StateKind, count, size int) (*TxChannel, error) {
	r, err := newRing(k, kind, 1, count, size, Private)
	if err != nil {
		return nil, err
	}
	return &TxChannel{Ring: r}, nil
}

// Put queues msg and returns the write pointer after it (pass it to
// WaitFor). If the ring is full it waits for the firmware, up to ctx.
func (c *TxChannel) Put(ctx context.Context, msg []byte) (uint32, error) {
	if len(msg) > c.size {
		return 0, fmt.Errorf("agx: %d-byte message on a %d-byte channel", len(msg), c.size)
	}
	next := (c.wptr + 1) % c.count
	for next == c.State.Buf.Load32(c.ptr(0, c.kind.ReadPtr)) {
		if err := sleep(ctx, 8*time.Millisecond); err != nil {
			return 0, fmt.Errorf("agx: channel full: %w", err)
		}
	}
	off := int(c.wptr) * c.size
	slot := c.Msgs.Bytes()[off : off+c.size]
	copy(slot, msg)
	clear(slot[len(msg):])
	// Store32 is a release store: the message is in memory before the
	// firmware can see the new write pointer.
	c.State.Buf.Store32(c.ptr(0, c.kind.WritePtr), next)
	c.wptr = next
	return next, nil
}

// WaitFor waits until the firmware has consumed everything up to wptr.
func (c *TxChannel) WaitFor(ctx context.Context, wptr uint32, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for c.State.Buf.Load32(c.ptr(0, c.kind.ReadPtr)) != wptr {
		if time.Now().After(deadline) {
			return fmt.Errorf("agx: firmware didn't consume the channel (read %d, want %d)",
				c.State.Buf.Load32(c.ptr(0, c.kind.ReadPtr)), wptr)
		}
		if err := sleep(ctx, time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Message sizes (Asahi: fw/channels.rs, 4 + *_SZ for the enum messages).
const (
	DeviceControlMsgSize = 0x30 // G13 (0x40 on G14 13.2+)
	EventMsgSize         = 0x38
	FwLogMsgSize         = 0x38
	FwLogPayloadSize     = 0xd8
	KTraceMsgSize        = 0x38
	FwCtlMsgSize         = 0x14
	PipeMsgSize          = 0x30
)

// StatsMsgSize depends on the firmware: 0x30 before 13.0b4, 0x40 after.
func StatsMsgSize(compat [3]uint32) int {
	if compat[0] >= 13 {
		return 0x40
	}
	return 0x30
}

// Channels are every channel the firmware expects (Asahi: make_mgr). The
// pipes are per priority (4) and kind (vertex, fragment, compute).
type Channels struct {
	DeviceControl *TxChannel
	FwCtl         *TxChannel
	Pipes         [4][3]*TxChannel
	Event         *RxChannel
	FwLog         *RxChannel
	KTrace        *RxChannel
	Stats         *RxChannel
	// FwLogPayload holds the log text: per sub-channel, 0x100 entries of
	// 0xd8 bytes (RawFwLogPayloadMsg), pointed to by fwlog_buf.
	FwLogPayload *Object
}

// NewChannels allocates them all.
func NewChannels(k *KernelAlloc, compat [3]uint32) (*Channels, error) {
	c := &Channels{}
	var err error
	tx := func(kind StateKind, count, size int) *TxChannel {
		if err != nil {
			return nil
		}
		var ch *TxChannel
		ch, err = NewTx(k, kind, count, size)
		return ch
	}
	rx := func(subs, count, size int) *RxChannel {
		if err != nil {
			return nil
		}
		var ch *RxChannel
		ch, err = NewRx(k, ChannelState, subs, count, size)
		return ch
	}
	c.DeviceControl = tx(ChannelState, 0x100, DeviceControlMsgSize)
	c.FwCtl = tx(FwCtlChannelState, 0x100, FwCtlMsgSize)
	for p := range c.Pipes {
		for kind := range c.Pipes[p] {
			c.Pipes[p][kind] = tx(ChannelState, 0x100, PipeMsgSize)
		}
	}
	c.Event = rx(1, 0x100, EventMsgSize)
	c.FwLog = rx(6, 0x100, FwLogMsgSize)
	c.KTrace = rx(1, 0x200, KTraceMsgSize)
	c.Stats = rx(1, 0x100, StatsMsgSize(compat))
	if err == nil {
		c.FwLogPayload, err = k.Alloc(Shared, 0x100*6*FwLogPayloadSize)
	}
	return c, err
}
