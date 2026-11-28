package simplefb

import (
	"context"
	"image"
	"image/color"
	"testing"
	"time"

	"godeep/kernel/gpu"
)

func TestDisplayLendsTheScreen(t *testing.T) {
	for _, format := range []Format{X8R8G8B8, X2R10G10B10} {
		fb := newFB(160, 120, format)
		c, err := NewConsole(fb, 1)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("console text"))
		before := fb.rgbaAt(1, 5) // inside the text area
		var d gpu.Display = status{c}
		if d.Size() != image.Pt(160, 120) {
			t.Fatalf("size %v", d.Size())
		}
		ctx, cancel := context.WithCancel(context.Background())
		s, err := d.NewSurface(ctx, 160, 120)
		if err != nil {
			t.Fatal(err)
		}
		red := color.RGBA{0xff, 0, 0, 0xff}
		gpu.Fill(s, s.Bounds(), red)
		if err := d.Show(ctx, s); err != nil {
			t.Fatal(err)
		}
		if got := fb.rgbaAt(80, 60); got != red {
			t.Fatalf("%s: shown pixel %v", format, got)
		}
		// console output while lent: off screen, kept as text
		c.Write([]byte("\nwhile lent"))
		if got := fb.rgbaAt(1, 5); got != red {
			t.Fatalf("%s: the console drew over the surface (%v)", format, got)
		}
		if st := c.Status(); !contains(st, "lent") {
			t.Fatalf("status %q", st)
		}
		// a present of part of the surface
		blue := color.RGBA{0, 0, 0xff, 0xff}
		gpu.Fill(s, image.Rect(10, 10, 20, 20), blue)
		s.Present(ctx, image.Rect(10, 10, 20, 20))
		if got := fb.rgbaAt(15, 15); got != blue {
			t.Fatalf("%s: presented pixel %v", format, got)
		}
		// the surface's owner ends: the console takes the screen back
		cancel()
		// wait through the console's lock (Resume holds it while it redraws)
		deadline := time.Now().Add(2 * time.Second)
		for contains(c.Status(), "lent") {
			if time.Now().After(deadline) {
				t.Fatalf("%s: console didn't resume", format)
			}
			time.Sleep(5 * time.Millisecond)
		}
		c.mu.Lock()
		got, mid := fb.rgbaAt(1, 5), fb.rgbaAt(80, 60)
		c.mu.Unlock()
		if got != before || mid == red {
			t.Fatalf("%s: text not redrawn (%v, was %v; middle %v)", format, got, before, mid)
		}
	}
}

func TestShowNilAndForeignSurface(t *testing.T) {
	fb := newFB(64, 48, X8R8G8B8)
	c, _ := NewConsole(fb, 1)
	d := c.Display()
	ctx := context.Background()
	s, _ := d.NewSurface(ctx, 64, 48)
	gpu.Fill(s, s.Bounds(), color.RGBA{0, 0xff, 0, 0xff})
	d.Show(ctx, s)
	d.Show(ctx, nil)
	if fb.rgbaAt(30, 30) == (color.RGBA{0, 0xff, 0, 0xff}) {
		t.Fatal("Show(nil) left the surface up")
	}
	// presenting a surface that isn't shown changes nothing
	s.Present(ctx, s.Bounds())
	if fb.rgbaAt(30, 30) == (color.RGBA{0, 0xff, 0, 0xff}) {
		t.Fatal("hidden surface reached the screen")
	}
	other, _ := NewConsole(newFB(64, 48, X8R8G8B8), 1)
	so, _ := other.Display().NewSurface(ctx, 8, 8)
	if d.Show(ctx, so) == nil {
		t.Fatal("showed another display's surface")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
