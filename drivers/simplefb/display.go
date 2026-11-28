package simplefb

import (
	"context"
	"errors"
	"image"
	"sync"

	"godeep/kernel/gpu"
)

// The framebuffer as a gpu.Display, so the compositor (kernel/wm) runs on
// a firmware framebuffer too: the Mac's (m1n1's) and Parallels' (UEFI GOP).
// While a surface is shown, the console keeps its text but draws into an
// off-screen copy (Suspend); when the surface goes away (Show(nil), or its
// ctx ends) the console takes the screen back and redraws (Resume).

// Suspend points the console's drawing at an off-screen buffer of the same
// geometry: the screen is someone else's until Resume.
func (c *Console) Suspend() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.screen != nil {
		return
	}
	c.screen = c.fb
	shadow := *c.fb
	shadow.Pix = make([]byte, len(c.fb.Pix))
	c.fb = &shadow
}

// Resume gives the console the screen back and redraws it.
func (c *Console) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.screen == nil {
		return
	}
	c.fb, c.screen = c.screen, nil
	c.ptrShown, c.under = false, nil // those pixels were the shadow's
	c.curShown = false
	c.fb.fill(0, c.fb.Height, c.bg)
	c.redraw()
	c.showCursor()
	c.showPointer()
}

// real is the framebuffer on screen (the console's, unless suspended).
func (c *Console) real() *Framebuffer {
	if c.screen != nil {
		return c.screen
	}
	return c.fb
}

type display struct {
	c     *Console
	mu    sync.Mutex
	shown *surface
}

// Display returns the framebuffer as a gpu.Display (one per console).
func (c *Console) Display() gpu.Display {
	c.dispOnce.Do(func() { c.disp = &display{c: c} })
	return c.disp
}

func (d *display) Size() image.Point {
	d.c.mu.Lock()
	defer d.c.mu.Unlock()
	f := d.c.real()
	return image.Pt(f.Width, f.Height)
}

func (d *display) NewSurface(ctx context.Context, w, h int) (gpu.Surface, error) {
	if w < 1 || h < 1 || w > 16384 || h > 16384 {
		return nil, errors.New("simplefb: bad surface size")
	}
	s := &surface{d: d, w: w, h: h, pix: make([]byte, 4*w*h)}
	context.AfterFunc(ctx, func() {
		d.mu.Lock()
		gone := d.shown == s
		if gone {
			d.shown = nil
		}
		d.mu.Unlock()
		if gone {
			d.c.Resume()
		}
	})
	return s, nil
}

func (d *display) Show(ctx context.Context, s gpu.Surface) error {
	if s == nil {
		d.mu.Lock()
		d.shown = nil
		d.mu.Unlock()
		d.c.Resume()
		return nil
	}
	sf, ok := s.(*surface)
	if !ok || sf.d != d {
		return errors.New("simplefb: not this display's surface")
	}
	d.c.Suspend()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.shown = sf
	d.blit(sf, sf.Bounds())
	return nil
}

// blit copies r of s to the screen, converting to the framebuffer's
// format. Called with d.mu held, while the console is suspended.
func (d *display) blit(s *surface, r image.Rectangle) {
	d.c.mu.Lock()
	f := d.c.real()
	d.c.mu.Unlock()
	r = r.Intersect(s.Bounds()).Intersect(image.Rect(0, 0, f.Width, f.Height))
	for y := r.Min.Y; y < r.Max.Y; y++ {
		src := s.pix[y*4*s.w+4*r.Min.X : y*4*s.w+4*r.Max.X]
		switch f.Format {
		case X8R8G8B8: // the surface's layout already
			copy(f.Pix[y*f.Stride+4*r.Min.X:], src)
		default:
			for i, x := 0, r.Min.X; x < r.Max.X; i, x = i+4, x+1 {
				f.set(x, y, f.RGB(src[i+2], src[i+1], src[i]))
			}
		}
	}
}

type surface struct {
	d    *display
	w, h int
	pix  []byte
}

func (s *surface) Bounds() image.Rectangle { return image.Rect(0, 0, s.w, s.h) }
func (s *surface) Pix() []byte             { return s.pix }
func (s *surface) Stride() int             { return 4 * s.w }

func (s *surface) Present(_ context.Context, r image.Rectangle) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if s.d.shown == s {
		s.d.blit(s, r)
	}
	return nil
}
