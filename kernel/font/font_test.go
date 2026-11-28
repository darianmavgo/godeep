package font

import (
	"image"
	"image/color"
	"sync"
	"testing"
)

func TestMonoMetrics(t *testing.T) {
	f, err := Mono(16)
	if err != nil {
		t.Fatal(err)
	}
	if f.Advance < 8 || f.Advance > 11 || f.Height < 18 || f.Ascent < 12 || f.Ascent >= f.Height {
		t.Fatalf("metrics: advance %d, ascent %d, height %d", f.Advance, f.Ascent, f.Height)
	}
	if w := f.Width("abc"); w != 3*f.Advance {
		t.Fatalf("Go Mono isn't monospace: Width(abc) = %d, advance %d", w, f.Advance)
	}
}

// coverage counts pixels a glyph changed, and partially covered ones.
func coverage(pix []byte) (changed, partial int) {
	for i := 0; i < len(pix); i += 4 {
		if pix[i+2] != 0 {
			changed++
			if pix[i+2] != 0xff {
				partial++
			}
		}
	}
	return
}

func TestGlyphAntialiased(t *testing.T) {
	f, err := Mono(20)
	if err != nil {
		t.Fatal(err)
	}
	const w, h = 40, 40
	pix := make([]byte, 4*w*h)
	adv := f.Glyph(pix, 4*w, image.Rect(0, 0, w, h), 4, 4, 'o', Regular, color.RGBA{0xff, 0xff, 0xff, 0xff})
	if adv != f.Advance {
		t.Fatalf("advance %d, want %d", adv, f.Advance)
	}
	changed, partial := coverage(pix)
	if changed < 20 || partial == 0 {
		t.Fatalf("'o': %d pixels drawn, %d partly: want an anti-aliased glyph", changed, partial)
	}
	// bold covers more
	pb := make([]byte, 4*w*h)
	f.Glyph(pb, 4*w, image.Rect(0, 0, w, h), 4, 4, 'o', Bold, color.RGBA{0xff, 0xff, 0xff, 0xff})
	if cb, _ := coverage(pb); cb <= changed {
		t.Fatalf("bold 'o' covers %d pixels, regular %d", cb, changed)
	}
	// a space draws nothing but advances
	ps := make([]byte, 4*w*h)
	if f.Glyph(ps, 4*w, image.Rect(0, 0, w, h), 4, 4, ' ', Regular, color.RGBA{0xff, 0, 0, 0xff}) != f.Advance {
		t.Fatal("space advance")
	}
	if c, _ := coverage(ps); c != 0 {
		t.Fatal("space drew pixels")
	}
}

func TestClipping(t *testing.T) {
	f, err := UI(24)
	if err != nil {
		t.Fatal(err)
	}
	const w, h = 30, 30
	pix := make([]byte, 4*w*h)
	// partly outside the buffer and the clip: must not panic or draw outside
	f.String(pix, 4*w, image.Rect(0, 0, 10, 10), -5, -8, "Wg█", Bold, color.RGBA{0xff, 0xff, 0xff, 0xff})
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if pix[4*(y*w+x)+2] != 0 && (x >= 10 || y >= 10) {
				t.Fatalf("drew outside the clip at %d,%d", x, y)
			}
		}
	}
	f.String(pix, 4*w, image.Rect(0, 0, 100, 100), 25, 25, "WWW", Regular, color.RGBA{0xff, 0xff, 0xff, 0xff})
}

func TestConcurrentUse(t *testing.T) {
	f, err := Mono(14)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pix := make([]byte, 4*200*20)
			f.String(pix, 4*200, image.Rect(0, 0, 200, 20), 0, 0, "hello, godeep ✓ λ", Style(i&1), color.RGBA{0xff, 0xff, 0xff, 0xff})
		}()
	}
	wg.Wait()
}
