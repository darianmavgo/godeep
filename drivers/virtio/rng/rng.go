// Package rng drives a virtio entropy device (virtio device ID 4,
// "virtio-rng"). QEMU backs it with the host's /dev/urandom, which makes it
// godeep's first real entropy source on QEMU: at start the driver hands a
// seed to the board (Options.Seed), which mixes it into the runtime's random
// generator (crypto/rand). It also publishes an *RNG for direct reads.
//
// Requests are rare, so they're polled to completion (no interrupt).
package rng

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	vmmio "godeep/drivers/virtio/mmio"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

// DeviceID is virtio-rng's device ID.
const DeviceID = 4

// SeedSize is how many bytes Options.Seed receives.
const SeedSize = 64

const maxRead = 256

// Options configure the driver.
type Options struct {
	// Seed, if set, is called with SeedSize bytes from the device each time
	// the driver starts.
	Seed func(seed []byte)
}

// Driver returns the virtio-rng driver for "virtio,device4" nodes.
func Driver(regs func(base uint64) mmio.Regs, opt Options) dev.Driver {
	return dev.Driver{Compatible: vmmio.Compatible(DeviceID), Run: func(ctx context.Context, d *dev.Device) error {
		if len(d.Reg) == 0 {
			return errors.New("virtio-rng: no registers")
		}
		return run(ctx, d, regs(d.Reg[0].Base), opt)
	}}
}

// RNG is the published service.
type RNG struct {
	reqs   chan *req
	down   chan struct{}
	bytes  atomic.Uint64
	seeded atomic.Bool
}

type req struct {
	p    []byte
	done chan result
}

type result struct {
	n   int
	err error
}

// Read fills p (up to 256 bytes per call) with device entropy.
func (r *RNG) Read(ctx context.Context, p []byte) (int, error) {
	q := &req{p: p[:min(len(p), maxRead)], done: make(chan result, 1)}
	select {
	case r.reqs <- q:
	case <-r.down:
		return 0, errors.New("virtio-rng: device stopped")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	res := <-q.done
	return res.n, res.err
}

func (r *RNG) Devices() []string {
	s := "entropy from the host"
	if r.seeded.Load() {
		s += ", seeded crypto/rand"
	}
	return []string{fmt.Sprintf("%s: %d bytes read", s, r.bytes.Load())}
}

func run(ctx context.Context, d *dev.Device, regs mmio.Regs, opt Options) error {
	t, err := vmmio.Init(ctx, regs, 0)
	if err != nil {
		return err
	}
	defer t.Reset()
	q, err := t.Queue(0, 8, d.DMA)
	if err != nil {
		return err
	}
	defer q.Free()
	buf, err := d.DMA.Alloc(maxRead, dma.DefaultAlign)
	if err != nil {
		return err
	}
	defer buf.Free()
	q.SuppressInterrupts(true)
	t.DriverOK()

	r := &RNG{reqs: make(chan *req), down: make(chan struct{})}
	defer close(r.down)
	fill := func(p []byte) (int, error) {
		if _, err := q.Add(buf.Phys, len(p), true); err != nil {
			return 0, err
		}
		q.Kick()
		deadline := time.Now().Add(time.Second)
		for {
			if _, n, ok := q.Used(); ok {
				n = min(n, len(p))
				copy(p, buf.Bytes[:n])
				r.bytes.Add(uint64(n))
				return n, nil
			}
			if time.Now().After(deadline) {
				return 0, errors.New("virtio-rng: request timed out")
			}
			t := time.NewTimer(50 * time.Microsecond)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return 0, ctx.Err()
			}
		}
	}

	if opt.Seed != nil {
		seed := make([]byte, SeedSize)
		for got := 0; got < len(seed); {
			n, err := fill(seed[got:])
			if err != nil {
				return err
			}
			got += n
		}
		opt.Seed(seed)
		r.seeded.Store(true)
	}
	d.Ready(r)
	for {
		select {
		case <-ctx.Done():
			return nil
		case rq := <-r.reqs:
			n, err := fill(rq.p)
			rq.done <- result{n, err}
		}
	}
}
