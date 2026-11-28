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
//
// The look is a Theme: Classic (flat, bitmap title font: the `wm` demo
// and its checks) or Night (anti-aliased fonts, a [x] close button,
// shadows, a gradient: the desktop). The close button only asks: the
// window's CloseRequests channel fires, and its owner decides (the
// desktop kills the terminal's process, which closes the window).
package wm

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"sync"

	"golang.org/x/image/font/basicfont"

	"godeep/kernel/font"
	"godeep/kernel/gpu"
	"godeep/kernel/input"
)

// Classic layout, pixels.
const (
	TitleH = 18 // title bar height
	Border = 1
)

// Classic colors.
var (
	Background = color.RGBA{0x2b, 0x3a, 0x4a, 0xff}
	TitleFocus = color.RGBA{0x3d, 0x7e, 0xd8, 0xff}
	TitleIdle  = color.RGBA{0x6a, 0x70, 0x78, 0xff}
	TitleText  = color.RGBA{0xff, 0xff, 0xff, 0xff}
	BorderCol  = color.RGBA{0x10, 0x14, 0x1c, 0xff}
)

// Theme is the compositor's look.
type Theme struct {
	Background, Background2   color.RGBA // top and bottom of the desktop (a gradient if they differ)
	TitleFocus, TitleIdle     color.RGBA
	TextFocus, TextIdle       color.RGBA
	BorderFocus, BorderIdle   color.RGBA
	Close, CloseHover, CloseX color.RGBA // the close button and its ×
	Shadow                    color.RGBA
	TitleH, BorderW           int
	ShadowOff                 int        // shadow offset; 0: none
	CloseButton               bool       // draw [x] (CloseRequests)
	Font                      *font.Face // title text; nil: the 7x13 bitmap font
	Hint                      string     // text at the bottom of the desktop
	HintColor                 color.RGBA
	Scale                     int // pointer size (1, or 2 on HiDPI)
	Gap                       int // cascade step for new windows
}

// Classic is the original look (the wm demo; check-wm's pixels).
var Classic = Theme{Background: Background, Background2: Background, TitleFocus: TitleFocus, TitleIdle: TitleIdle,
	TextFocus: TitleText, TextIdle: TitleText, BorderFocus: BorderCol, BorderIdle: BorderCol,
	TitleH: TitleH, BorderW: Border, Scale: 1, Gap: 40}

// Night is a dark theme after Tokyo Night, for the desktop, scaled for
// HiDPI screens (scale 2 on the Mac's 3456x2234).
func Night(scale int) (Theme, error) {
	scale = max(1, scale)
	f, err := font.UI(float64(14 * scale))
	if err != nil {
		return Theme{}, err
	}
	return Theme{
		Background: color.RGBA{0x1f, 0x23, 0x35, 0xff}, Background2: color.RGBA{0x10, 0x11, 0x1a, 0xff},
		TitleFocus: color.RGBA{0x24, 0x28, 0x3b, 0xff}, TitleIdle: color.RGBA{0x16, 0x16, 0x1e, 0xff},
		TextFocus: color.RGBA{0xc0, 0xca, 0xf5, 0xff}, TextIdle: color.RGBA{0x56, 0x5f, 0x89, 0xff},
		BorderFocus: color.RGBA{0x7a, 0xa2, 0xf7, 0xff}, BorderIdle: color.RGBA{0x29, 0x2e, 0x42, 0xff},
		Close: color.RGBA{0xf7, 0x76, 0x8e, 0xff}, CloseHover: color.RGBA{0xff, 0x9e, 0xb0, 0xff},
		CloseX: color.RGBA{0x1a, 0x1b, 0x26, 0xff}, Shadow: color.RGBA{0x08, 0x09, 0x0e, 0xff},
		TitleH: 28 * scale, BorderW: scale, ShadowOff: 6 * scale, CloseButton: true, Font: f,
		HintColor: color.RGBA{0x41, 0x48, 0x68, 0xff}, Scale: scale, Gap: 36 * scale,
	}, nil
}

// Options configure a Manager.
type Options struct {
	Theme Theme
	// Keys, if set, sees every key event first, on the compositor's
	// goroutine; returning true keeps it from the focused window. It must
	// not call the Manager (that would deadlock): signal another goroutine.
	Keys func(input.Event) bool
	// Desktop, if set, is called (on the compositor's goroutine) with the
	// point of a left click on the empty desktop. Same rule as Keys.
	Desktop func(image.Point)
}

// ErrClosed is returned once the compositor has stopped.
var ErrClosed = errors.New("wm: compositor stopped")

// Manager is the compositor.
type Manager struct {
	d    gpu.Display
	opt  Options
	th   *Theme
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
	hover   *Window // its close button is under the pointer
	next    int     // cascade position for new windows
	damage  image.Rectangle
	ctx     context.Context
}

// New returns a compositor for d with the Classic theme; Run starts it.
func New(d gpu.Display) *Manager { return NewWith(d, Options{Theme: Classic}) }

// NewWith returns a compositor for d with options.
func NewWith(d gpu.Display, opt Options) *Manager {
	m := &Manager{d: d, opt: opt, req: make(chan func(), 64), done: make(chan struct{})}
	m.th = &m.opt.Theme
	if m.th.Scale < 1 {
		m.th.Scale = 1
	}
	if m.th.Gap == 0 {
		m.th.Gap = 40
	}
	return m
}

// Theme is the compositor's theme.
func (m *Manager) Theme() Theme { return *m.th }

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

// Size is the screen's size (known once Run has started).
func (m *Manager) Size() image.Point { return m.d.Size() }

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

// Focused is the window with the keyboard focus (nil: none).
func (m *Manager) Focused(ctx context.Context) *Window {
	var w *Window
	m.do(ctx, func() { w = m.focus })
	return w
}

// Window is a client's window (a gpu.Surface).
type Window struct {
	m        *Manager
	title    string
	w, h     int
	pix      []byte // the client's
	shown    []byte // the compositor's copy
	at       image.Point
	events   chan input.Event
	closeReq chan struct{}
	closed   bool
}

// NewWindow opens a w x h window; it closes when ctx ends.
func (m *Manager) NewWindow(ctx context.Context, title string, w, h int) (*Window, error) {
	if w < 1 || h < 1 || w > 8192 || h > 8192 {
		return nil, errors.New("wm: bad window size")
	}
	win := &Window{m: m, title: title, w: w, h: h, pix: make([]byte, 4*w*h), shown: make([]byte, 4*w*h),
		events: make(chan input.Event, 64), closeReq: make(chan struct{}, 1)}
	err := m.do(ctx, func() {
		n := m.next % 8
		m.next++
		g := m.th.Gap
		win.at = image.Pt(g+g*n, g+m.th.TitleH+g*n)
		// keep it on screen when the screen is small
		if sz := m.size; sz.X > 0 {
			win.at.X = max(m.th.BorderW, min(win.at.X, sz.X-w-m.th.BorderW))
			win.at.Y = max(m.th.TitleH+m.th.BorderW, min(win.at.Y, sz.Y-h-m.th.BorderW))
		}
		m.wins = append(m.wins, win)
		m.setFocus(win)
		m.damage = m.damage.Union(win.outer())
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

// CloseRequests receives when the close button is clicked. Closing is the
// owner's decision: end the window's ctx.
func (w *Window) CloseRequests() <-chan struct{} { return w.closeReq }

// CloseButton is where the close button is on screen (empty without one).
func (w *Window) CloseButton(ctx context.Context) (image.Rectangle, error) {
	var r image.Rectangle
	err := w.m.do(ctx, func() { r = w.closeBox() })
	return r, err
}

// SetTitle changes the title bar's text.
func (w *Window) SetTitle(ctx context.Context, title string) error {
	return w.m.do(ctx, func() {
		if w.title != title && !w.closed {
			w.title = title
			w.m.damage = w.m.damage.Union(w.titleBar())
		}
	})
}

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
	c, th := w.content(), w.m.th
	return image.Rect(c.Min.X-th.BorderW, c.Min.Y-th.TitleH, c.Max.X+th.BorderW, c.Min.Y)
}

// frame is everything the window covers: border, title bar, contents.
func (w *Window) frame() image.Rectangle {
	c, th := w.content(), w.m.th
	return image.Rect(c.Min.X-th.BorderW, c.Min.Y-th.TitleH-th.BorderW, c.Max.X+th.BorderW, c.Max.Y+th.BorderW)
}

// outer is the frame plus its shadow: what to redraw when the window changes.
func (w *Window) outer() image.Rectangle {
	f := w.frame()
	if o := w.m.th.ShadowOff; o > 0 {
		f.Max = f.Max.Add(image.Pt(o, o))
	}
	return f
}

// closeBox is the close button's square, at the right of the title bar.
func (w *Window) closeBox() image.Rectangle {
	if !w.m.th.CloseButton {
		return image.Rectangle{}
	}
	t := w.titleBar()
	d := t.Dy() * 5 / 9
	x1 := t.Max.X - (t.Dy()-d)/2 - w.m.th.BorderW
	y0 := t.Min.Y + (t.Dy()-d)/2 + w.m.th.BorderW/2
	return image.Rect(x1-d, y0, x1, y0+d)
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
	if m.hover == win {
		m.hover = nil
	}
	if m.focus == win {
		m.focus = nil
		if n := len(m.wins); n > 0 {
			m.setFocus(m.wins[n-1])
		}
	}
	m.damage = m.damage.Union(win.outer())
}

func (m *Manager) setFocus(win *Window) {
	if m.focus != nil {
		m.damage = m.damage.Union(m.focus.frame())
	}
	m.focus = win
	if win != nil {
		m.damage = m.damage.Union(win.frame())
	}
}

func (m *Manager) raise(win *Window) {
	for i, x := range m.wins {
		if x == win && i != len(m.wins)-1 {
			m.wins = append(append(m.wins[:i], m.wins[i+1:]...), win)
			m.damage = m.damage.Union(win.outer())
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

func (m *Manager) pointerRect(p image.Point) image.Rectangle {
	s := m.th.Scale
	return image.Rect(p.X, p.Y, p.X+11*s, p.Y+len(arrow)*s)
}

func (m *Manager) movePointer(p image.Point) {
	p.X = min(max(p.X, 0), m.size.X-1)
	p.Y = min(max(p.Y, 0), m.size.Y-1)
	m.damage = m.damage.Union(m.pointerRect(m.ptr))
	m.ptr, m.ptrOn = p, true
	m.damage = m.damage.Union(m.pointerRect(p))
	if m.drag != nil {
		m.damage = m.damage.Union(m.drag.outer())
		m.drag.at = p.Sub(m.dragOff)
		m.damage = m.damage.Union(m.drag.outer())
		return
	}
	w := m.under(p)
	hover := w
	if w == nil || !p.In(w.closeBox()) {
		hover = nil
	}
	if hover != m.hover {
		for _, x := range []*Window{m.hover, hover} {
			if x != nil {
				m.damage = m.damage.Union(x.closeBox())
			}
		}
		m.hover = hover
	}
	if w != nil && p.In(w.content()) {
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
		if e.Down && e.Code == 0 && w == nil && m.opt.Desktop != nil {
			m.opt.Desktop(m.ptr)
			return
		}
		if e.Down && e.Code == 0 && w != nil {
			m.raise(w)
			m.setFocus(w)
			if m.ptr.In(w.closeBox()) {
				select {
				case w.closeReq <- struct{}{}:
				default:
				}
				return
			}
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
		if m.opt.Keys != nil && m.opt.Keys(e) {
			return
		}
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
	m.background(r)
	for _, w := range m.wins {
		m.drawWindow(w, r)
	}
	if m.ptrOn {
		m.drawPointer(r)
	}
	m.screen.Present(m.ctx, r)
}

// background paints the desktop in r: flat, or a vertical gradient, and
// the hint text.
func (m *Manager) background(r image.Rectangle) {
	th := m.th
	if th.Background == th.Background2 {
		gpu.Fill(m.screen, r, th.Background)
	} else {
		h := max(1, m.size.Y-1)
		lerp := func(a, b uint8, y int) uint8 { return uint8((int(a)*(h-y) + int(b)*y) / h) }
		for y := r.Min.Y; y < r.Max.Y; y++ {
			a, b := th.Background, th.Background2
			gpu.Fill(m.screen, image.Rect(r.Min.X, y, r.Max.X, y+1),
				color.RGBA{lerp(a.R, b.R, y), lerp(a.G, b.G, y), lerp(a.B, b.B, y), 0xff})
		}
	}
	if th.Hint != "" && th.Font != nil {
		w := th.Font.Width(th.Hint)
		x, y := (m.size.X-w)/2, m.size.Y-th.Font.Height*2
		if image.Rect(x, y, x+w, y+th.Font.Height).Overlaps(r) {
			th.Font.String(m.screen.Pix(), m.screen.Stride(), r, x, y, th.Hint, font.Regular, th.HintColor)
		}
	}
}

func (m *Manager) drawWindow(w *Window, clip image.Rectangle) {
	th := m.th
	if !w.outer().Overlaps(clip) {
		return
	}
	focused := w == m.focus
	f := w.frame()
	if th.ShadowOff > 0 {
		s := f.Add(image.Pt(th.ShadowOff, th.ShadowOff))
		// only the part not under the window itself
		for _, part := range []image.Rectangle{
			image.Rect(f.Max.X, s.Min.Y, s.Max.X, s.Max.Y),
			image.Rect(s.Min.X, f.Max.Y, f.Max.X, s.Max.Y),
		} {
			gpu.Fill(m.screen, part.Intersect(clip), th.Shadow)
		}
	}
	border := th.BorderIdle
	if focused {
		border = th.BorderFocus
	}
	gpu.Fill(m.screen, f.Intersect(clip), border)
	bar := w.titleBar()
	bar.Min.X += th.BorderW
	bar.Max.X -= th.BorderW
	bar.Min.Y += th.BorderW
	title, text := th.TitleIdle, th.TextIdle
	if focused {
		title, text = th.TitleFocus, th.TextFocus
	}
	gpu.Fill(m.screen, bar.Intersect(clip), title)
	tclip := bar.Intersect(clip)
	if cb := w.closeBox(); !cb.Empty() {
		tclip.Max.X = min(tclip.Max.X, cb.Min.X-th.BorderW*4)
	}
	if th.Font != nil {
		x := bar.Min.X + 10*th.Scale
		y := bar.Min.Y + (bar.Dy()-th.Font.Height)/2
		th.Font.String(m.screen.Pix(), m.screen.Stride(), tclip, x, y, w.title, font.Regular, text)
	} else {
		drawText(m.screen, bar.Min.X+6, bar.Min.Y+(bar.Dy()-basicfont.Face7x13.Height)/2, w.title, text, tclip)
	}
	if cb := w.closeBox(); !cb.Empty() && cb.Overlaps(clip) {
		c := th.Close
		if w == m.hover {
			c = th.CloseHover
		}
		if !focused && w != m.hover {
			c = th.TextIdle
		}
		drawCloseButton(m.screen, cb, c, th.CloseX, clip)
	}
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

// drawCloseButton draws an anti-aliased disc in box with an × across it.
func drawCloseButton(s gpu.Surface, box image.Rectangle, disc, x color.RGBA, clip image.Rectangle) {
	cx, cy := float64(box.Min.X+box.Max.X)/2, float64(box.Min.Y+box.Max.Y)/2
	rad := float64(box.Dx()) / 2
	arm := rad * 0.42
	thick := max(1.0, rad*0.16)
	r := box.Intersect(clip)
	pix, stride := s.Pix(), s.Stride()
	for py := r.Min.Y; py < r.Max.Y; py++ {
		for px := r.Min.X; px < r.Max.X; px++ {
			fx, fy := float64(px)+0.5-cx, float64(py)+0.5-cy
			// disc coverage: 1 inside, fading over one pixel at the edge
			d := rad - hypot(fx, fy)
			a := min(1, max(0, d+0.5))
			if a == 0 {
				continue
			}
			col := disc
			// the ×: distance to either diagonal, within the arm length
			if abs(fx) <= arm+0.5 && abs(fy) <= arm+0.5 {
				dd := min(abs(fx-fy), abs(fx+fy)) / 1.41421356
				if xa := min(1, max(0, thick/2-dd+0.5)); xa > 0 {
					col = mix(disc, x, xa)
				}
			}
			p := pix[py*stride+4*px:]
			under := color.RGBA{p[2], p[1], p[0], 0xff}
			out := mix(under, col, a)
			p[0], p[1], p[2] = out.B, out.G, out.R
		}
	}
}

func hypot(x, y float64) float64 { return math.Sqrt(x*x + y*y) }

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	l := func(x, y uint8) uint8 { return uint8(float64(x)*(1-t) + float64(y)*t + 0.5) }
	return color.RGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), 0xff}
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

func (m *Manager) drawPointer(clip image.Rectangle) {
	s := m.th.Scale
	for dy, row := range arrow {
		for dx, ch := range row {
			var c color.RGBA
			switch ch {
			case '#':
				c = color.RGBA{0, 0, 0, 0xff}
			case '.':
				c = color.RGBA{0xff, 0xff, 0xff, 0xff}
			default:
				continue
			}
			q := m.ptr.Add(image.Pt(dx*s, dy*s))
			gpu.Fill(m.screen, image.Rect(q.X, q.Y, q.X+s, q.Y+s).Intersect(clip), c)
		}
	}
}
