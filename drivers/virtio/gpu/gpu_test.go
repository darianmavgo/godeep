package gpu

import (
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"sync"
	"testing"
	"time"

	vmmio "godeep/drivers/virtio/mmio"
	"godeep/drivers/virtio/mmio/virtiotest"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	kgpu "godeep/kernel/gpu"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
)

const dmaBase = 0x1_2000_0000

// host models QEMU's side: resources with their own pixels, filled only by
// TRANSFER_TO_HOST_2D from the attached backing.
type host struct {
	mu       sync.Mutex
	w, h     uint32
	res      map[uint32]*resource
	scanout  uint32
	flushes  int
	commands []uint32
}

type resource struct {
	w, h    uint32
	backing uint64
	pix     []byte
}

func (h *host) handle(d *virtiotest.Device, cmd []byte, rsp []byte) int {
	le := binary.LittleEndian
	typ := le.Uint32(cmd)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commands = append(h.commands, typ)
	ok := func() int { le.PutUint32(rsp, respOKNoData); return hdrSize }
	fail := func() int { le.PutUint32(rsp, respErrUnspec); return hdrSize }
	switch typ {
	case cmdGetDisplayInfo:
		clear(rsp[:displayInfoSize])
		le.PutUint32(rsp, respOKDisplayInfo)
		m := rsp[hdrSize:]
		le.PutUint32(m[8:], h.w)
		le.PutUint32(m[12:], h.h)
		le.PutUint32(m[16:], 1)
		return displayInfoSize
	case cmdResourceCreate:
		id, f, w, hh := le.Uint32(cmd[24:]), le.Uint32(cmd[28:]), le.Uint32(cmd[32:]), le.Uint32(cmd[36:])
		if f != formatB8G8R8X8 || h.res[id] != nil || id == 0 {
			return fail()
		}
		h.res[id] = &resource{w: w, h: hh, pix: make([]byte, w*hh*4)}
		return ok()
	case cmdAttachBacking:
		r := h.res[le.Uint32(cmd[24:])]
		if r == nil || le.Uint32(cmd[28:]) != 1 || le.Uint32(cmd[40:]) != r.w*r.h*4 {
			return fail()
		}
		r.backing = le.Uint64(cmd[32:])
		return ok()
	case cmdDetachBacking:
		r := h.res[le.Uint32(cmd[24:])]
		if r == nil {
			return fail()
		}
		r.backing = 0
		return ok()
	case cmdResourceUnref:
		id := le.Uint32(cmd[24:])
		if h.res[id] == nil || h.scanout == id {
			return fail() // unref while showing would be a driver bug
		}
		delete(h.res, id)
		return ok()
	case cmdSetScanout:
		id := le.Uint32(cmd[44:])
		if id != 0 && h.res[id] == nil {
			return fail()
		}
		h.scanout = id
		return ok()
	case cmdTransferToHost:
		x, y, w, hh := le.Uint32(cmd[24:]), le.Uint32(cmd[28:]), le.Uint32(cmd[32:]), le.Uint32(cmd[36:])
		off, id := le.Uint64(cmd[40:]), le.Uint32(cmd[48:])
		r := h.res[id]
		if r == nil || r.backing == 0 || x+w > r.w || y+hh > r.h || off != uint64(y*r.w*4+x*4) {
			return fail()
		}
		src := d.At(r.backing, int(r.w*r.h*4))
		for row := y; row < y+hh; row++ {
			o := row*r.w*4 + x*4
			copy(r.pix[o:o+w*4], src[o:o+w*4])
		}
		return ok()
	case cmdResourceFlush:
		if h.res[le.Uint32(cmd[40:])] == nil {
			return fail()
		}
		h.flushes++
		return ok()
	}
	return fail()
}

func (h *host) at(x, y int) color.RGBA {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.res[h.scanout]
	p := r.pix[(y*int(r.w)+x)*4:]
	return color.RGBA{p[2], p[1], p[0], 0xff}
}

func setup(t *testing.T) (*Display, *host, context.Context) {
	h := &host{w: 640, h: 480, res: map[uint32]*resource{}}
	m := virtiotest.New(DeviceID, 0, make([]byte, 24), make([]byte, 16<<20), dmaBase)
	m.OnNotify = func(d *virtiotest.Device, q int) {
		for {
			chain, ok := d.PopChain(q)
			if !ok {
				return
			}
			if len(chain) != 2 || chain[0].Write || !chain[1].Write {
				t.Errorf("chain %+v", chain)
				return
			}
			n := h.handle(d, d.At(chain[0].Addr, chain[0].Len), d.At(chain[1].Addr, chain[1].Len))
			d.Push(q, chain[0].ID, n)
		}
	}
	dev.Register(Driver(func(uint64) mmio.Regs { return m.F }))
	mgr := dev.NewManager(proc.NewTable(), nil, dma.New(dma.Region{Base: dmaBase, Mem: m.Mem}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	mgr.Start(ctx, []dev.Node{{Name: "gpu0", Compatible: vmmio.Compatible(DeviceID), Reg: []dev.Range{{Base: 0xa000000}}}})
	svc, err := mgr.Get(ctx, "gpu0")
	if err != nil {
		t.Fatal(err)
	}
	return svc.(*Display), h, ctx
}

func TestTestPatternReachesTheHost(t *testing.T) {
	d, h, ctx := setup(t)
	if d.Size() != image.Pt(640, 480) {
		t.Fatalf("size %v", d.Size())
	}
	sctx, release := context.WithCancel(ctx)
	s, err := d.NewSurface(sctx, 640, 480)
	if err != nil {
		t.Fatal(err)
	}
	kgpu.TestPattern(s)
	if err := d.Show(ctx, s); err != nil {
		t.Fatal(err)
	}
	for i, want := range kgpu.Bars {
		x := (2*i + 1) * 640 / 16 // the middle of bar i
		if got := h.at(x, 100); got != want {
			t.Errorf("bar %d at x=%d: %v, want %v", i, x, got, want)
		}
	}
	if got := h.at(639, 479); got != (color.RGBA{255, 255, 255, 255}) {
		t.Errorf("ramp end %v", got)
	}

	// Damage: only the presented rectangle reaches the host.
	kgpu.Fill(s, image.Rect(0, 0, 640, 480), color.RGBA{0x10, 0x20, 0x30, 0xff})
	if err := s.Present(ctx, image.Rect(10, 10, 20, 20)); err != nil {
		t.Fatal(err)
	}
	if got := h.at(15, 15); got != (color.RGBA{0x10, 0x20, 0x30, 0xff}) {
		t.Errorf("damaged pixel %v", got)
	}
	if got := h.at(30, 30); got != kgpu.Bars[0] {
		t.Errorf("undamaged pixel changed: %v", got)
	}

	// The owner goes away: the surface leaves the screen and the host.
	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		n, shown := len(h.res), h.scanout
		h.mu.Unlock()
		if n == 0 && shown == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after release: %d resources, scanout %d", n, shown)
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.Present(ctx, s.Bounds()); err == nil {
		t.Error("present after release")
	}
	if got := d.Devices()[0]; got != "640x480 scanout 0, showing resource 0, 0 surfaces, 2 flushes" {
		t.Errorf("Devices: %q", got)
	}
}

func TestBadSurfaces(t *testing.T) {
	d, _, ctx := setup(t)
	for _, sz := range []image.Point{{0, 10}, {10, -1}, {5000, 5000}} {
		if _, err := d.NewSurface(ctx, sz.X, sz.Y); err == nil {
			t.Errorf("surface %v accepted", sz)
		}
	}
	if err := d.Show(ctx, nil); err != nil {
		t.Errorf("blank: %v", err)
	}
}
