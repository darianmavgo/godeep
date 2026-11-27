// Package gpu drives a virtio GPU in 2D mode (virtio device ID 16,
// QEMU's virtio-gpu-device), and publishes it as a gpu.Display.
//
// The 2D protocol runs on the control queue: GET_DISPLAY_INFO for the
// preferred size; per surface RESOURCE_CREATE_2D and
// RESOURCE_ATTACH_BACKING (the pixels live in our DMA memory); SET_SCANOUT
// to show one; per frame TRANSFER_TO_HOST_2D + RESOURCE_FLUSH of the
// damaged rectangle. One goroutine owns the device; requests reach it
// over a channel and are polled to completion (commands are rare and
// short, and polling sidesteps the HVF interrupt bug).
package gpu

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"sync/atomic"
	"time"

	vmmio "godeep/drivers/virtio/mmio"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	kgpu "godeep/kernel/gpu"
	"godeep/kernel/mmio"
)

// DeviceID is virtio-gpu's device ID.
const DeviceID = 16

// Control commands and responses (virtio spec 5.7.6.7).
const (
	cmdGetDisplayInfo  = 0x0100
	cmdResourceCreate  = 0x0101
	cmdResourceUnref   = 0x0102
	cmdSetScanout      = 0x0103
	cmdResourceFlush   = 0x0104
	cmdTransferToHost  = 0x0105
	cmdAttachBacking   = 0x0106
	cmdDetachBacking   = 0x0107
	respOKNoData       = 0x1100
	respOKDisplayInfo  = 0x1101
	respErrUnspec      = 0x1200
	formatB8G8R8X8     = 2 // bytes B, G, R, X: XRGB8888 little-endian
	hdrSize            = 24
	maxScanouts        = 16
	displayInfoSize    = hdrSize + maxScanouts*24
	maxSurfacePixels   = 4096 * 4096
	commandTimeout     = 2 * time.Second
	defaultW, defaultH = 1024, 768
)

// Driver returns the virtio-gpu driver for "virtio,device10" nodes.
func Driver(regs func(base uint64) mmio.Regs) dev.Driver {
	return dev.Driver{Compatible: vmmio.Compatible(DeviceID), Run: func(ctx context.Context, d *dev.Device) error {
		if len(d.Reg) == 0 {
			return errors.New("virtio-gpu: no registers")
		}
		return run(ctx, d, regs(d.Reg[0].Base))
	}}
}

// Display is the published service: a gpu.Display on scanout 0.
type Display struct {
	reqs chan request
	down chan struct{}
	size image.Point
	dev  *device // touched only by request closures, on the owner goroutine

	surfaces, flushes atomic.Int64
	showing           atomic.Uint32 // resource on screen (0: none)
}

var _ kgpu.Display = (*Display)(nil)

type request struct {
	do   func() error // runs on the owner goroutine
	done chan error
}

// call runs fn on the device's goroutine.
func (d *Display) call(ctx context.Context, fn func() error) error {
	r := request{do: fn, done: make(chan error, 1)}
	select {
	case d.reqs <- r:
	case <-d.down:
		return errors.New("virtio-gpu: device stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-r.done:
		return err
	case <-d.down:
		return errors.New("virtio-gpu: device stopped")
	}
}

func (d *Display) Size() image.Point { return d.size }

// Devices reports the display for lsdev.
func (d *Display) Devices() []string {
	return []string{fmt.Sprintf("%dx%d scanout 0, showing resource %d, %d surfaces, %d flushes",
		d.size.X, d.size.Y, d.showing.Load(), d.surfaces.Load(), d.flushes.Load())}
}

// Surface is a host resource backed by DMA memory.
type Surface struct {
	d    *Display
	id   uint32
	w, h int
	buf  *dma.Buffer
	gone atomic.Bool
}

func (s *Surface) Bounds() image.Rectangle { return image.Rect(0, 0, s.w, s.h) }
func (s *Surface) Pix() []byte             { return s.buf.Bytes[:s.w*s.h*4] }
func (s *Surface) Stride() int             { return 4 * s.w }

// Present copies damage to the host and flushes it to the screen.
func (s *Surface) Present(ctx context.Context, damage image.Rectangle) error {
	r := damage.Intersect(s.Bounds())
	if r.Empty() {
		return nil
	}
	if s.gone.Load() {
		return errors.New("virtio-gpu: surface released")
	}
	return s.d.call(ctx, func() error { return s.d.dev.present(s, r) })
}

// NewSurface creates a w×h surface, freed when ctx ends.
func (d *Display) NewSurface(ctx context.Context, w, h int) (kgpu.Surface, error) {
	if w <= 0 || h <= 0 || w*h > maxSurfacePixels {
		return nil, fmt.Errorf("virtio-gpu: bad surface size %dx%d", w, h)
	}
	var s *Surface
	err := d.call(ctx, func() error {
		var err error
		s, err = d.dev.create(w, h)
		return err
	})
	if err != nil {
		return nil, err
	}
	d.surfaces.Add(1)
	// The owner's lifetime is the surface's: a killed program's pixels
	// leave the screen and its memory goes back to the DMA pool.
	context.AfterFunc(ctx, func() {
		s.gone.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		if d.call(ctx, func() error { return d.dev.release(s) }) == nil {
			d.surfaces.Add(-1)
		}
	})
	return s, nil
}

// Show puts s on scanout 0 (nil blanks it).
func (d *Display) Show(ctx context.Context, s kgpu.Surface) error {
	if s == nil {
		return d.call(ctx, func() error { return d.dev.scanout(nil) })
	}
	vs, ok := s.(*Surface)
	if !ok || vs.d != d {
		return errors.New("virtio-gpu: not a surface of this display")
	}
	if vs.gone.Load() {
		return errors.New("virtio-gpu: surface released")
	}
	return d.call(ctx, func() error { return d.dev.scanout(vs) })
}

// device is the owner goroutine's state.
type device struct {
	ctx      context.Context
	q        *vmmio.Queue
	a        *dma.Allocator
	cmd, rsp *dma.Buffer
	nextID   uint32
	shown    *Surface
	disp     *Display
}

func run(ctx context.Context, d *dev.Device, regs mmio.Regs) error {
	t, err := vmmio.Init(ctx, regs, 0)
	if err != nil {
		return err
	}
	defer t.Reset()
	q, err := t.Queue(0, 64, d.DMA) // controlq (1 is the cursor queue: unused)
	if err != nil {
		return err
	}
	defer q.Free()
	cmd, err := d.DMA.Alloc(4096, dma.DefaultAlign)
	if err != nil {
		return err
	}
	defer cmd.Free()
	rsp, err := d.DMA.Alloc(4096, dma.DefaultAlign)
	if err != nil {
		return err
	}
	defer rsp.Free()
	q.SuppressInterrupts(true)
	t.DriverOK()

	g := &device{ctx: ctx, q: q, a: d.DMA, cmd: cmd, rsp: rsp, nextID: 1}
	size, err := g.displayInfo()
	if err != nil {
		return err
	}
	disp := &Display{reqs: make(chan request), down: make(chan struct{}), size: size}
	disp.dev = g
	g.disp = disp
	defer close(disp.down)
	d.Ready(disp)
	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-disp.reqs:
			r.done <- r.do()
		}
	}
}

// submit sends the command in g.cmd[:n] and waits for a response of
// rspLen bytes, returning the response type.
func (g *device) submit(n, rspLen int) (uint32, error) {
	le := binary.LittleEndian
	le.PutUint32(g.rsp.Bytes[0:], 0)
	if _, err := g.q.AddChain([]vmmio.Seg{
		{Phys: g.cmd.Bus, Len: n},
		{Phys: g.rsp.Bus, Len: rspLen, Write: true},
	}); err != nil {
		return 0, err
	}
	g.q.Kick()
	deadline := time.Now().Add(commandTimeout)
	wait := 20 * time.Microsecond
	for {
		if _, _, ok := g.q.Used(); ok {
			typ := le.Uint32(g.rsp.Bytes[0:])
			if typ >= respErrUnspec {
				return typ, fmt.Errorf("virtio-gpu: command %#x failed: %#x", le.Uint32(g.cmd.Bytes[0:]), typ)
			}
			return typ, nil
		}
		if time.Now().After(deadline) {
			return 0, errors.New("virtio-gpu: command timed out")
		}
		tm := time.NewTimer(wait)
		select {
		case <-tm.C:
		case <-g.ctx.Done():
			tm.Stop()
			return 0, g.ctx.Err()
		}
		wait = min(2*wait, time.Millisecond)
	}
}

// header writes a control header for cmd and clears the rest of the
// command buffer up to n bytes; it returns the buffer to fill.
func (g *device) header(cmd uint32, n int) []byte {
	b := g.cmd.Bytes[:n]
	clear(b)
	binary.LittleEndian.PutUint32(b[0:], cmd)
	return b
}

func putRect(b []byte, r image.Rectangle) {
	le := binary.LittleEndian
	le.PutUint32(b[0:], uint32(r.Min.X))
	le.PutUint32(b[4:], uint32(r.Min.Y))
	le.PutUint32(b[8:], uint32(r.Dx()))
	le.PutUint32(b[12:], uint32(r.Dy()))
}

func (g *device) simple(n int) error {
	typ, err := g.submit(n, hdrSize)
	if err != nil {
		return err
	}
	if typ != respOKNoData {
		return fmt.Errorf("virtio-gpu: unexpected response %#x", typ)
	}
	return nil
}

func (g *device) displayInfo() (image.Point, error) {
	g.header(cmdGetDisplayInfo, hdrSize)
	typ, err := g.submit(hdrSize, displayInfoSize)
	if err != nil {
		return image.Point{}, err
	}
	if typ != respOKDisplayInfo {
		return image.Point{}, fmt.Errorf("virtio-gpu: display info: response %#x", typ)
	}
	le := binary.LittleEndian
	for i := 0; i < maxScanouts; i++ {
		m := g.rsp.Bytes[hdrSize+24*i:]
		w, h, enabled := le.Uint32(m[8:]), le.Uint32(m[12:]), le.Uint32(m[16:])
		if enabled != 0 && w > 0 && h > 0 {
			return image.Pt(int(w), int(h)), nil
		}
	}
	return image.Pt(defaultW, defaultH), nil
}

func (g *device) create(w, h int) (*Surface, error) {
	buf, err := g.a.Alloc(w*h*4, 4096)
	if err != nil {
		return nil, err
	}
	clear(buf.Bytes)
	id := g.nextID
	g.nextID++
	le := binary.LittleEndian
	b := g.header(cmdResourceCreate, 40)
	le.PutUint32(b[24:], id)
	le.PutUint32(b[28:], formatB8G8R8X8)
	le.PutUint32(b[32:], uint32(w))
	le.PutUint32(b[36:], uint32(h))
	if err := g.simple(40); err != nil {
		buf.Free()
		return nil, err
	}
	b = g.header(cmdAttachBacking, 48)
	le.PutUint32(b[24:], id)
	le.PutUint32(b[28:], 1) // one entry: the buffer is contiguous
	le.PutUint64(b[32:], buf.Bus)
	le.PutUint32(b[40:], uint32(w*h*4))
	if err := g.simple(48); err != nil {
		g.unref(id)
		buf.Free()
		return nil, err
	}
	return &Surface{d: g.disp, id: id, w: w, h: h, buf: buf}, nil
}

func (g *device) unref(id uint32) error {
	b := g.header(cmdResourceUnref, 32)
	binary.LittleEndian.PutUint32(b[24:], id)
	return g.simple(32)
}

func (g *device) release(s *Surface) error {
	if g.shown == s {
		if err := g.scanout(nil); err != nil {
			return err
		}
	}
	b := g.header(cmdDetachBacking, 32)
	binary.LittleEndian.PutUint32(b[24:], s.id)
	if err := g.simple(32); err != nil {
		return err
	}
	if err := g.unref(s.id); err != nil {
		return err
	}
	s.buf.Free() // the host no longer reads it
	return nil
}

func (g *device) scanout(s *Surface) error {
	le := binary.LittleEndian
	b := g.header(cmdSetScanout, 48)
	var id uint32
	if s != nil {
		putRect(b[24:], s.Bounds())
		id = s.id
	}
	le.PutUint32(b[40:], 0) // scanout 0
	le.PutUint32(b[44:], id)
	if err := g.simple(48); err != nil {
		return err
	}
	g.shown = s
	g.disp.showing.Store(id)
	if s != nil {
		return g.present(s, s.Bounds())
	}
	return nil
}

func (g *device) present(s *Surface, r image.Rectangle) error {
	le := binary.LittleEndian
	b := g.header(cmdTransferToHost, 56)
	putRect(b[24:], r)
	le.PutUint64(b[40:], uint64(r.Min.Y*s.Stride()+4*r.Min.X))
	le.PutUint32(b[48:], s.id)
	if err := g.simple(56); err != nil {
		return err
	}
	b = g.header(cmdResourceFlush, 48)
	putRect(b[24:], r)
	le.PutUint32(b[40:], s.id)
	if err := g.simple(48); err != nil {
		return err
	}
	g.disp.flushes.Add(1)
	return nil
}
