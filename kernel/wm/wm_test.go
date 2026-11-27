package wm

import (
	"context"
	"image"
	"image/color"
	"sync"
	"testing"
	"time"

	"godeep/kernel/gpu"
	"godeep/kernel/input"
)

// fakeDisplay keeps the shown surface's last presented pixels.
type fakeDisplay struct {
	mu    sync.Mutex
	shown *fakeSurface
}

type fakeSurface struct {
	d        *fakeDisplay
	w, h     int
	pix      []byte
	screen   []byte // what "reached the display" (copied on Present)
	presents int
}

func (d *fakeDisplay) Size() image.Point { return image.Pt(640, 480) }
func (d *fakeDisplay) NewSurface(_ context.Context, w, h int) (gpu.Surface, error) {
	return &fakeSurface{d: d, w: w, h: h, pix: make([]byte, 4*w*h), screen: make([]byte, 4*w*h)}, nil
}
func (d *fakeDisplay) Show(_ context.Context, s gpu.Surface) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.shown = s.(*fakeSurface)
	return nil
}

func (s *fakeSurface) Bounds() image.Rectangle { return image.Rect(0, 0, s.w, s.h) }
func (s *fakeSurface) Pix() []byte             { return s.pix }
func (s *fakeSurface) Stride() int             { return 4 * s.w }
func (s *fakeSurface) Present(_ context.Context, r image.Rectangle) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		o := y*4*s.w + 4*r.Min.X
		copy(s.screen[o:o+4*r.Dx()], s.pix[o:])
	}
	s.presents++
	return nil
}

func (d *fakeDisplay) at(x, y int) color.RGBA {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shown == nil {
		return color.RGBA{}
	}
	p := d.shown.screen[y*4*d.shown.w+4*x:]
	return color.RGBA{p[2], p[1], p[0], 0xff}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

func start(t *testing.T) (*Manager, *fakeDisplay, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	d := &fakeDisplay{}
	m := New(d)
	done := make(chan error)
	go func() { done <- m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return m, d, ctx
}

var red = color.RGBA{0xff, 0, 0, 0xff}
var green = color.RGBA{0, 0xff, 0, 0xff}

func TestComposeFocusDrag(t *testing.T) {
	m, d, ctx := start(t)
	a, err := m.NewWindow(ctx, "a", 100, 80)
	if err != nil {
		t.Fatal(err)
	}
	gpu.Fill(a, a.Bounds(), red)
	a.Present(ctx, a.Bounds())
	pa, _ := a.Position()
	eventually(t, "a on screen", func() bool { return d.at(pa.X+50, pa.Y+40) == red })
	if c := d.at(5, 5); c != Background {
		t.Fatalf("background %v", c)
	}
	if c := d.at(pa.X+50, pa.Y-TitleH/2); c != TitleFocus && c != TitleText {
		t.Fatalf("title bar %v", c)
	}

	// b opens on top, overlapping a, and takes the focus
	bctx, closeB := context.WithCancel(ctx)
	b, _ := m.NewWindow(bctx, "b", 100, 80)
	gpu.Fill(b, b.Bounds(), green)
	b.Present(ctx, b.Bounds())
	pb, _ := b.Position()
	eventually(t, "b on top", func() bool { return d.at(pb.X+5, pb.Y+5) == green })

	// keys go to the focused window
	m.Input(input.Event{Kind: input.KeyDown, Code: 4})
	if e := <-b.Events(); e.Kind != input.KeyDown {
		t.Fatalf("b got %v", e)
	}

	// a click on a's visible part raises and focuses it (and reaches it)
	to := func(p image.Point) input.Event {
		return input.Event{Kind: input.Absolute, X: int32(p.X * input.AbsMax / 639), Y: int32(p.Y * input.AbsMax / 479)}
	}
	click := pa.Add(image.Pt(5, 5)) // a's corner, under b? a is at pa, b at pb = pa+40
	m.Input(to(click))
	m.Input(input.Event{Kind: input.Button, Code: 0, Down: true})
	m.Input(input.Event{Kind: input.Button, Code: 0, Down: false})
	eventually(t, "a raised", func() bool { return d.at(pb.X+5, pb.Y+5) == red })
	gotClick := false
	for len(a.Events()) > 0 {
		if e := <-a.Events(); e.Kind == input.Button {
			gotClick = true
		}
	}
	if !gotClick {
		t.Fatal("a didn't get its click")
	}

	// drag a by its title bar 150 px right
	grab := pa.Add(image.Pt(30, -TitleH/2))
	m.Input(to(grab))
	m.Input(input.Event{Kind: input.Button, Code: 0, Down: true})
	m.Input(to(grab.Add(image.Pt(150, 0))))
	m.Input(input.Event{Kind: input.Button, Code: 0, Down: false})
	eventually(t, "a moved", func() bool {
		p, _ := a.Position()
		return p.X >= pa.X+148 && p.X <= pa.X+152 && d.at(p.X+50, p.Y+40) == red
	})

	// b closes with its ctx: green is gone
	closeB()
	eventually(t, "b closed", func() bool { return d.at(pb.X+60, pb.Y+70) != green })
}

func TestManyClients(t *testing.T) {
	m, _, ctx := start(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wctx, cancel := context.WithCancel(ctx)
			defer cancel()
			w, err := m.NewWindow(wctx, "w", 50, 50)
			if err != nil {
				t.Error(err)
				return
			}
			for f := 0; f < 50; f++ {
				gpu.Fill(w, w.Bounds(), color.RGBA{uint8(i * 30), uint8(f), 0, 0xff})
				if err := w.Present(wctx, w.Bounds()); err != nil {
					t.Error(err)
					return
				}
				m.Input(input.Event{Kind: input.Motion, DX: 1})
			}
		}(i)
	}
	wg.Wait()
}
