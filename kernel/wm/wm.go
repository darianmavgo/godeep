// Package wm is a compositor: windows that client processes draw into with
// the CPU, stacked on one gpu.Display, with title bars, a pointer,
// click-to-focus and drag-to-move. Key events go to the focused window,
// pointer events to the window under the pointer.
//
// One goroutine (Run) owns the scene, like a driver owns its device; the
// methods hand it work over a channel. A Window is a gpu.Surface: the
// client draws into Pix and calls Present, which copies the damaged part
// into the compositor's copy before returning, so the client can go on
// drawing while the screen is composed. A window goes away with the ctx it
// was created with (its process ended).
package wm

import (
	"context"
	"errors"
	"image"
	"image/color"
	"sync"

	"golang.org/x/image/font/basicfont"

	"godeep/kernel/gpu"
	"godeep/kernel/input"
)

// Layout, pixels.
const (
	TitleH = 18 // title bar height
	Border = 1
)

var (
	Background = color.RGBA{0x2b, 0x3a, 0x4a, 0xff}
	TitleFocus = color.RGBA{0x3d, 0x7e, 0xd8, 0xff}
	TitleIdle  = color.RGBA{0x6a, 0x70, 0x78, 0xff}
	TitleText  = color.RGBA{0xff, 0xff, 0xff, 0xff}
	BorderCol  = color.RGBA{0x10, 0x14, 0x1c, 0xff}
)

// ErrClosed is returned once the compositor has stopped.
var ErrClosed = errors.New("wm: compositor stopped")

// Manager is the compositor.
type Manager struct {
	d    gpu.Display
	req  chan func()
	done chan struct{}

	// owned by Run's goroutine
	screen  gpu.Surface
	size    image.Point
	wins    []*Window // bottom to top
	focus   *Window
	ptr     image.Point
	ptrOn   bool
	drag    *Window
	dragOff image.Point
	next    int // cascade position for new windows
	damage  image.Rectangle
	ctx     context.Context
}

// New returns a compositor for d; Run starts it.
func New(d gpu.Display) *Manager {
	return &Manager{d: d, req: make(chan func(), 64), done: make(chan struct{})}
}

// Run shows the desktop and composes until ctx ends.
func (m *Manager) Run(ctx context.Context) error {
	defer close(m.done)
	m.ctx = ctx
	m.size = m.d.Size()
	s, err := m.d.NewSurface(ctx, m.size.X, m.size.Y)
	if err != nil {
		return err
	}
	m.screen = s
	m.ptr = m.size.Div(2)
	m.damage = s.Bounds()
	m.flush()
	if err := m.d.Show(ctx, s); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case f := <-m.req:
			f()
			// take whatever else is queued before composing
			for more := true; more; {
				select {
				case f := <-m.req:
					f()
				default:
					more = false
				}
			}
			m.flush()
		}
	}
}

// do runs f on the compositor's goroutine and waits for it.
func (m *Manager) do(ctx context.Context, f func()) error {
	ack := make(chan struct{})
	select {
	case m.req <- func() { f(); close(ack) }:
	case <-m.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-m.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Input hands the compositor one event from an input device. It doesn't
// block: events beyond what the queue holds are dropped.
func (m *Manager) Input(e input.Event) {
	select {
	case m.req <- func() { m.handle(e) }:
	default:
	}
}

// Window is a client's window (a gpu.Surface).
type Window struct {
	m      *Manager
	title  string
	w, h   int
	pix    []byte // the client's
	shown  []byte // the compositor's copy
	at     image.Point
	events chan input.Event
	closed bool
}

// NewWindow opens a w x h window; it closes when ctx ends.
func (m *Manager) NewWindow(ctx context.Context, title string, w, h int) (*Window, error) {
	if w < 1 || h < 1 || w > 8192 || h > 8192 {
		return nil, errors.New("wm: bad window size")
	}
	win := &Window{m: m, title: title, w: w, h: h, pix: make([]byte, 4*w*h), shown: make([]byte, 4*w*h),
		events: make(chan input.Event, 64)}
	err := m.do(ctx, func() {
		n := m.next % 8
		m.next++
		win.at = image.Pt(40+40*n, 40+TitleH+40*n)
		m.wins = append(m.wins, win)
		m.setFocus(win)
		m.damage = m.damage.Union(win.frame())
	})
	if err != nil {
		return nil, err
	}
	var once sync.Once
	context.AfterFunc(ctx, func() {
		once.Do(func() {
			select {
			case m.req <- func() { m.remove(win) }:
			case <-m.done:
			}
		})
	})
	return win, nil
}

func (w *Window) Bounds() image.Rectangle { return image.Rect(0, 0, w.w, w.h) }
func (w *Window) Pix() []byte             { return w.pix }
func (w *Window) Stride() int             { return 4 * w.w }

// Events are the window's input events: keys while it has the focus,
// pointer events (Absolute with window coordinates, Button, Wheel) while
// the pointer is over it.
func (w *Window) Events() <-chan input.Event { return w.events }

// Present shows the damaged part of the window's pixels.
func (w *Window) Present(ctx context.Context, damage image.Rectangle) error {
	damage = damage.Intersect(w.Bounds())
	if damage.Empty() {
		return nil
	}
	return w.m.do(ctx, func() {
		if w.closed {
			return
		}
		for y := damage.Min.Y; y < damage.Max.Y; y++ {
			o := y*4*w.w + 4*damage.Min.X
			copy(w.shown[o:o+4*damage.Dx()], w.pix[o:])
		}
		w.m.damage = w.m.damage.Union(damage.Add(w.at))
	})
}

// Position is where the window's contents are on screen (for tests).
func (w *Window) Position() (image.Point, error) {
	var p image.Point
	err := w.m.do(context.Background(), func() { p = w.at })
	return p, err
}

// --- the compositor's goroutine ----------------------------------------

func (w *Window) content() image.Rectangle { return image.Rect(0, 0, w.w, w.h).Add(w.at) }

func (w *Window) titleBar() image.Rectangle {
	c := w.content()
	return image.Rect(c.Min.X-Border, c.Min.Y-TitleH, c.Max.X+Border, c.Min.Y)
}

// frame is everything the window covers: border, title bar, contents.
func (w *Window) frame() image.Rectangle {
	c := w.content()
	return image.Rect(c.Min.X-Border, c.Min.Y-TitleH-Border, c.Max.X+Border, c.Max.Y+Border)
}

func (m *Manager) remove(win *Window) {
	for i, x := range m.wins {
		if x == win {
			m.wins = append(m.wins[:i], m.wins[i+1:]...)
			break
		}
	}
	win.closed = true
	if m.drag == win {
		m.drag = nil
	}
	if m.focus == win {
		m.focus = nil
		if n := len(m.wins); n > 0 {
			m.setFocus(m.wins[n-1])
		}
	}
	m.damage = m.damage.Union(win.frame())
}

func (m *Manager) setFocus(win *Window) {
	if m.focus != nil {
		m.damage = m.damage.Union(m.focus.titleBar())
	}
	m.focus = win
	if win != nil {
		m.damage = m.damage.Union(win.titleBar())
	}
}

func (m *Manager) raise(win *Window) {
	for i, x := range m.wins {
		if x == win && i != len(m.wins)-1 {
			m.wins = append(append(m.wins[:i], m.wins[i+1:]...), win)
			m.damage = m.damage.Union(win.frame())
			return
		}
	}
}

// under is the topmost window whose frame contains p.
func (m *Manager) under(p image.Point) *Window {
	for i := len(m.wins) - 1; i >= 0; i-- {
		if p.In(m.wins[i].frame()) {
			return m.wins[i]
		}
	}
	return nil
}

func send(w *Window, e input.Event) {
	select {
	case w.events <- e:
	default: // a slow client loses events, the compositor doesn't wait
	}
}

func (m *Manager) movePointer(p image.Point) {
	p.X = min(max(p.X, 0), m.size.X-1)
	p.Y = min(max(p.Y, 0), m.size.Y-1)
	m.damage = m.damage.Union(pointerRect(m.ptr))
	m.ptr, m.ptrOn = p, true
	m.damage = m.damage.Union(pointerRect(p))
	if m.drag != nil {
		m.damage = m.damage.Union(m.drag.frame())
		m.drag.at = p.Sub(m.dragOff)
		m.damage = m.damage.Union(m.drag.frame())
		return
	}
	if w := m.under(p); w != nil && p.In(w.content()) {
		l := p.Sub(w.at)
		send(w, input.Event{Kind: input.Absolute, X: int32(l.X), Y: int32(l.Y)})
	}
}

func (m *Manager) handle(e input.Event) {
	switch e.Kind {
	case input.Absolute:
		m.movePointer(image.Pt(int(e.X)*(m.size.X-1)/input.AbsMax, int(e.Y)*(m.size.Y-1)/input.AbsMax))
	case input.Motion:
		m.movePointer(m.ptr.Add(image.Pt(int(e.DX), int(e.DY))))
	case input.Button:
		w := m.under(m.ptr)
		if e.Down && e.Code == 0 && w != nil {
			m.raise(w)
			m.setFocus(w)
			if m.ptr.In(w.titleBar()) {
				m.drag, m.dragOff = w, m.ptr.Sub(w.at)
				return
			}
		}
		if !e.Down && m.drag != nil {
			m.drag = nil
			return
		}
		if w != nil && m.ptr.In(w.content()) {
			send(w, e)
		}
	case input.Wheel:
		if w := m.under(m.ptr); w != nil {
			send(w, e)
		}
	case input.KeyDown, input.KeyUp:
		if m.focus != nil {
			send(m.focus, e)
		}
	}
}

// flush composes and presents what changed.
func (m *Manager) flush() {
	r := m.damage.Intersect(m.screen.Bounds())
	m.damage = image.Rectangle{}
	if r.Empty() {
		return
	}
	gpu.Fill(m.screen, r, Background)
	for _, w := range m.wins {
		m.drawWindow(w, r)
	}
	if m.ptrOn {
		drawPointer(m.screen, m.ptr, r)
	}
	m.screen.Present(m.ctx, r)
}

func (m *Manager) drawWindow(w *Window, clip image.Rectangle) {
	f := w.frame()
	if !f.Overlaps(clip) {
		return
	}
	gpu.Fill(m.screen, f.Intersect(clip), BorderCol)
	bar := w.titleBar().Inset(Border)
	bar.Max.Y += Border
	col := TitleIdle
	if w == m.focus {
		col = TitleFocus
	}
	gpu.Fill(m.screen, bar.Intersect(clip), col)
	drawText(m.screen, bar.Min.X+6, bar.Min.Y+(bar.Dy()-basicfont.Face7x13.Height)/2, w.title, TitleText, bar.Intersect(clip))
	c := w.content().Intersect(clip)
	if c.Empty() {
		return
	}
	pix, stride := m.screen.Pix(), m.screen.Stride()
	for y := c.Min.Y; y < c.Max.Y; y++ {
		src := (y-w.at.Y)*4*w.w + 4*(c.Min.X-w.at.X)
		copy(pix[y*stride+4*c.Min.X:], w.shown[src:src+4*c.Dx()])
	}
}

// DrawText draws str at x, y (top left) in s with the 7x13 bitmap font:
// for clients writing into their windows.
func DrawText(s gpu.Surface, x, y int, str string, c color.RGBA) {
	drawText(s, x, y, str, c, s.Bounds())
}

// drawText draws s with the 7x13 bitmap font, clipped.
func drawText(s gpu.Surface, x, y int, str string, c color.RGBA, clip image.Rectangle) {
	face := basicfont.Face7x13
	for _, r := range str {
		idx := -1
		for _, rg := range face.Ranges {
			if r >= rg.Low && r < rg.High {
				idx = rg.Offset + int(r-rg.Low)
				break
			}
		}
		if idx >= 0 {
			for gy := 0; gy < face.Height; gy++ {
				for gx := 0; gx < face.Width; gx++ {
					if _, _, _, a := face.Mask.At(gx, idx*face.Height+gy).RGBA(); a > 0x7fff {
						if p := image.Pt(x+gx, y+gy); p.In(clip) {
							gpu.Set(s, p.X, p.Y, c)
						}
					}
				}
			}
		}
		x += face.Advance
	}
}

// The pointer: an arrow, '#' black outline, '.' white fill.
var arrow = []string{
	"#",
	"##",
	"#.#",
	"#..#",
	"#...#",
	"#....#",
	"#.....#",
	"#......#",
	"#.......#",
	"#........#",
	"#.....#####",
	"#..#..#",
	"#.# #..#",
	"##  #..#",
	"#    #..#",
	"     ####",
}

func pointerRect(p image.Point) image.Rectangle {
	return image.Rect(p.X, p.Y, p.X+11, p.Y+len(arrow))
}

func drawPointer(s gpu.Surface, p image.Point, clip image.Rectangle) {
	for dy, row := range arrow {
		for dx, ch := range row {
			q := p.Add(image.Pt(dx, dy))
			if !q.In(clip) {
				continue
			}
			switch ch {
			case '#':
				gpu.Set(s, q.X, q.Y, color.RGBA{0, 0, 0, 0xff})
			case '.':
				gpu.Set(s, q.X, q.Y, color.RGBA{0xff, 0xff, 0xff, 0xff})
			}
		}
	}
}
