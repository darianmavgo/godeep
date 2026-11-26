package simplefb

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"strings"
	"testing"
)

func newFB(w, h int, f Format) *Framebuffer {
	bpp := 4
	if f == R5G6B5 {
		bpp = 2
	}
	stride := w*bpp + 64 // padded rows, as real hardware often has
	return &Framebuffer{Pix: make([]byte, stride*h), Width: w, Height: h, Stride: stride, Format: f}
}

func lit(f *Framebuffer) (n int) {
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			if f.rgbaAt(x, y).R > 0x80 {
				n++
			}
		}
	}
	return
}

func TestFormatsAndScroll(t *testing.T) {
	for _, form := range []Format{X8R8G8B8, X2R10G10B10, R5G6B5} {
		fb := newFB(320, 100, form)
		c, err := NewConsole(fb, 1)
		if err != nil {
			t.Fatal(err)
		}
		if lit(fb) != 0 {
			t.Fatalf("%s: screen not cleared", form)
		}
		fmt.Fprint(c, "godeep$ ps")
		if lit(fb) == 0 {
			t.Fatalf("%s: nothing drawn", form)
		}
		// fill and scroll well past the bottom; nothing may panic and the
		// last line must be visible
		_, rows := c.Size()
		for i := 0; i < rows*3; i++ {
			fmt.Fprintf(c, "line %d\n", i)
		}
		fmt.Fprint(c, "LAST")
		if c.row != rows-1 || lit(fb) == 0 {
			t.Fatalf("%s: scroll state row=%d", form, c.row)
		}
	}
	if _, err := NewConsole(&Framebuffer{Width: 10, Height: 10, Stride: 40, Format: "y8"}, 1); err == nil {
		t.Fatal("bad format accepted")
	}
}

// Render a sample screen to a PNG for eyeballing:
//
//	SIMPLEFB_PNG=/tmp/fb.png go test -run TestRenderPNG ./drivers/simplefb/
func TestRenderPNG(t *testing.T) {
	out := os.Getenv("SIMPLEFB_PNG")
	if out == "" {
		t.Skip("set SIMPLEFB_PNG to write a PNG")
	}
	fb := newFB(1200, 360, X2R10G10B10)
	c, _ := NewConsole(fb, 1)
	fmt.Fprint(c, strings.Join([]string{
		"godeep 0.1.0 (tamago/arm64, go1.27.4-X:simd, GOMAXPROCS=1)",
		"board: apple-m1pro (J316s); entered at EL2, dropped to EL1; device tree from m1n1",
		"godeep$ ps",
		"PID  PPID  NAME          STATE    UPTIME  GOROUTINES  GOROUTINE STATES",
		"0    -     [kernel]      running  2.1s    3           2 chan receive, 1 select",
		"1    0     devmgr        running  2.1s    1           1 chan receive",
		"2    0     init          running  2.1s    1           1 select",
		"3    2     sh            running  2.1s    1           1 running",
		"godeep$ ",
	}, "\n"))
	img := image.NewRGBA(image.Rect(0, 0, fb.Width, fb.Height))
	for y := 0; y < fb.Height; y++ {
		for x := 0; x < fb.Width; x++ {
			img.SetRGBA(x, y, fb.rgbaAt(x, y))
		}
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	png.Encode(f, img)
}
