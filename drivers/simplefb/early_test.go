package simplefb

import (
	"image/color"
	"image/png"
	"os"
	"testing"
)

func newEarly(t *testing.T, w, h int, f Format) (*Framebuffer, *Early) {
	fb := newFB(w, h, f)
	e, ok := NewEarly(fb.Pix, fb.Width, fb.Height, fb.Stride, []byte(f))
	if !ok {
		t.Fatalf("NewEarly rejected %dx%d %s", w, h, f)
	}
	return fb, &e
}

func near(a, b color.RGBA) bool {
	d := func(x, y uint8) bool { return x-y < 9 || y-x < 9 }
	return d(a.R, b.R) && d(a.G, b.G) && d(a.B, b.B)
}

func TestEarlyMarks(t *testing.T) {
	for _, f := range []Format{X8R8G8B8, X2R10G10B10, R5G6B5} {
		fb, e := newEarly(t, 960, 540, f)
		for n := 1; n <= 3; n++ {
			e.Mark(n)
		}
		for n := 1; n <= 4; n++ {
			x, y := e.MarkOrigin(n)
			c := markColors[n-1]
			got := fb.rgbaAt(x+1, y+1) // corner: outside the digit
			want := color.RGBA{c[0], c[1], c[2], 255}
			if n <= 3 && !near(got, want) {
				t.Errorf("%s: mark %d corner = %v, want %v", f, n, got, want)
			}
			if n == 4 && got != (color.RGBA{0, 0, 0, 255}) {
				t.Errorf("%s: mark 4 drawn without being reached: %v", f, got)
			}
		}
	}
}

func TestEarlyFault(t *testing.T) {
	fb, e := newEarly(t, 1280, 720, X2R10G10B10)
	e.Mark(1)
	e.Fault(2, 0x96000045, 0x10100123456, 0xdead0000)
	red := fb.rgbaAt(fb.Width-1, fb.Height-1)
	if !near(red, color.RGBA{0x90, 0x10, 0x10, 255}) {
		t.Fatalf("background = %v, want red", red)
	}
	if n := lit(fb); n < 2000 { // text is white: R > 0x80 everywhere, but so is red 0x90...
		t.Fatalf("only %d lit pixels", n)
	}
	white := 0
	for y := 0; y < fb.Height; y++ {
		for x := 0; x < fb.Width; x++ {
			if c := fb.rgbaAt(x, y); c.G > 0xf0 && c.B > 0xf0 {
				white++
			}
		}
	}
	if white < 2000 {
		t.Fatalf("fault text: %d white pixels, want thousands", white)
	}
	if out := os.Getenv("SIMPLEFB_PNG"); out != "" {
		f, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		png.Encode(f, fb.Image())
	}
}

func TestEarlyRejects(t *testing.T) {
	pix := make([]byte, 100*4*10)
	for _, c := range []struct {
		w, h, stride int
		f            string
	}{
		{100, 10, 400, "y8"},       // unknown format
		{100, 11, 400, "x8r8g8b8"}, // too tall for the buffer
		{100, 10, 399, "x8r8g8b8"}, // stride shorter than a row
		{0, 10, 400, "x8r8g8b8"},
	} {
		if _, ok := NewEarly(pix, c.w, c.h, c.stride, []byte(c.f)); ok {
			t.Errorf("accepted %+v", c)
		}
	}
	var zero Early // not Valid: drawing is a no-op, never a panic
	zero.Mark(1)
	zero.Fault(1, 0, 0, 0)
}

// Drawing past the edges is clipped, never a panic: the fault screen must
// not fault.
func TestEarlyClips(t *testing.T) {
	_, e := newEarly(t, 40, 20, R5G6B5)
	for n := 1; n <= 20; n++ {
		e.Mark(n)
	}
	e.Fault(99, ^uint64(0), ^uint64(0), ^uint64(0))
	e.Rect(-10, -10, 1000, 1000, 0xffff)
}

func TestEarlyNoAllocs(t *testing.T) {
	_, e := newEarly(t, 320, 200, X8R8G8B8)
	if n := testing.AllocsPerRun(20, func() {
		e.Mark(3)
		e.Fault(3, 1, 2, 3)
		NewEarly(e.Pix, e.Width, e.Height, e.Stride, []byte("x2r10g10b10"))
	}); n != 0 {
		t.Fatalf("early drawing allocates (%v per run); it must not", n)
	}
}
