// Package p9 drives a virtio 9P transport (virtio device ID 9): QEMU's
// host directory sharing (-fsdev local,... -device virtio-9p-device,
// mount_tag=host). It mounts the share with fs/ninep and publishes it as a
// volume named by its tag, so the shell reads host:/path.
//
// One goroutine owns the queue; requests come to it over a channel, one at
// a time, and are polled to completion (9P round trips are short).
package p9

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	vmmio "godeep/drivers/virtio/mmio"
	"godeep/fs/ninep"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

// DeviceID is virtio-9p's device ID.
const DeviceID = 9

// FeatureMountTag: the configuration holds the share's tag.
const FeatureMountTag = 1

// MSize is the largest 9P message (QEMU accepts up to several MB).
const MSize = 128 << 10

const rpcTimeout = 10 * time.Second

// Driver returns the virtio-9p driver for "virtio,device9" nodes.
func Driver(regs func(base uint64) mmio.Regs) dev.Driver {
	return dev.Driver{Compatible: vmmio.Compatible(DeviceID), Run: func(ctx context.Context, d *dev.Device) error {
		if len(d.Reg) == 0 {
			return errors.New("virtio-9p: no registers")
		}
		return run(ctx, d, regs(d.Reg[0].Base))
	}}
}

// Volume is the published service: the mounted share.
type Volume struct {
	*ninep.Client
	tag      string
	requests *atomic.Uint64
}

// VolumeName is the share's tag: the shell's volume name (host:/...).
func (v *Volume) VolumeName() string { return v.tag }

func (v *Volume) Devices() []string {
	return []string{fmt.Sprintf("9P share %q, msize %d, %d requests", v.tag, MSize, v.requests.Load())}
}

type transport struct {
	reqs chan *rpcReq
	down <-chan struct{}
}

type rpcReq struct {
	msg  []byte
	done chan rpcResult
}

type rpcResult struct {
	resp []byte
	err  error
}

func (t *transport) MaxMessage() int { return MSize }

func (t *transport) RPC(ctx context.Context, msg []byte) ([]byte, error) {
	r := &rpcReq{msg: msg, done: make(chan rpcResult, 1)}
	select {
	case t.reqs <- r:
	case <-t.down:
		return nil, errors.New("virtio-9p: device stopped")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	res := <-r.done
	return res.resp, res.err
}

func run(ctx context.Context, d *dev.Device, r mmio.Regs) error {
	t, err := vmmio.Init(ctx, r, FeatureMountTag)
	if err != nil {
		return err
	}
	defer t.Reset()
	tag := "host"
	if t.Has(FeatureMountTag) {
		n := int(t.Config32(0) & 0xffff) // tag_len, then the tag
		b := make([]byte, n)
		for i := range b {
			b[i] = t.Config8(2 + uint64(i))
		}
		tag = string(b)
	}
	q, err := t.Queue(0, 32, d.DMA)
	if err != nil {
		return err
	}
	defer q.Free()
	req, err := d.DMA.Alloc(MSize, dma.DefaultAlign)
	if err != nil {
		return err
	}
	defer req.Free()
	resp, err := d.DMA.Alloc(MSize, dma.DefaultAlign)
	if err != nil {
		return err
	}
	defer resp.Free()
	q.SuppressInterrupts(true)
	t.DriverOK()

	var requests atomic.Uint64
	rpc := func(msg []byte) ([]byte, error) {
		if len(msg) > MSize {
			return nil, fmt.Errorf("virtio-9p: %d-byte request", len(msg))
		}
		copy(req.Bytes, msg)
		if _, err := q.AddChain([]vmmio.Seg{{Phys: req.Phys, Len: len(msg)}, {Phys: resp.Phys, Len: MSize, Write: true}}); err != nil {
			return nil, err
		}
		q.Kick()
		requests.Add(1)
		deadline := time.Now().Add(rpcTimeout)
		for spins := 0; ; spins++ {
			if _, n, ok := q.Used(); ok {
				n = min(n, MSize)
				return append([]byte(nil), resp.Bytes[:n]...), nil
			}
			if spins < 200 {
				continue
			}
			if time.Now().After(deadline) {
				return nil, errors.New("virtio-9p: request timed out")
			}
			tm := time.NewTimer(20 * time.Microsecond)
			select {
			case <-tm.C:
			case <-ctx.Done():
				tm.Stop()
				return nil, ctx.Err()
			}
		}
	}

	down := make(chan struct{})
	defer close(down)
	tp := &transport{reqs: make(chan *rpcReq), down: down}
	// This goroutine owns the queue; the mount below talks to it.
	d.Proc.Go(func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-down:
				return
			case r := <-tp.reqs:
				b, err := rpc(r.msg)
				r.done <- rpcResult{b, err}
			}
		}
	})
	c, err := ninep.Mount(ctx, tp, tag)
	if err != nil {
		return err
	}
	d.Ready(&Volume{Client: c, tag: tag, requests: &requests})
	<-ctx.Done()
	return nil
}
