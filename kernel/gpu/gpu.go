// Package gpu holds godeep's display capability: a Display puts Surfaces
// on screen. A driver (drivers/virtio/gpu on QEMU) publishes a Display as
// its device service; a program draws into a Surface's pixels with the CPU
// and Presents the rectangle it changed.
//
// A Surface belongs to whoever created it: when the ctx given to
// NewSurface ends (the process is killed, or returns), the driver frees
// it, and takes it off the screen if it was showing.
package gpu

import (
	"context"
	"image"
	"image/color"
)

// Surface is a drawable image: 32-bit pixels, B, G, R, X bytes in memory
// (little-endian XRGB8888, the order simplefb and virtio-gpu share).
type Surface interface {
	Bounds() image.Rectangle
	Pix() []byte
	Stride() int // bytes per row
	// Present copies the damaged rectangle to the display (if this surface
	// is showing) and returns once the device has it.
	Present(ctx context.Context, damage image.Rectangle) error
}

// Display is the authority to put surfaces on screen.
type Display interface {
	// Size is the preferred mode: the display's size in pixels.
	Size() image.Point
	NewSurface(ctx context.Context, w, h int) (Surface, error)
	// Show puts s on screen (nil: blank the display).
	Show(ctx context.Context, s Surface) error
}

// Set writes one pixel.
func Set(s Surface, x, y int, c color.RGBA) {
	if !(image.Point{x, y}.In(s.Bounds())) {
		return
	}
	p := s.Pix()[y*s.Stride()+4*x:]
	p[0], p[1], p[2], p[3] = c.B, c.G, c.R, 0xff
}

// At reads one pixel.
func At(s Surface, x, y int) color.RGBA {
	p := s.Pix()[y*s.Stride()+4*x:]
	return color.RGBA{R: p[2], G: p[1], B: p[0], A: 0xff}
}

// Fill paints a rectangle.
func Fill(s Surface, r image.Rectangle, c color.RGBA) {
	r = r.Intersect(s.Bounds())
	if r.Empty() {
		return
	}
	row := make([]byte, 4*r.Dx())
	for i := 0; i < len(row); i += 4 {
		row[i], row[i+1], row[i+2], row[i+3] = c.B, c.G, c.R, 0xff
	}
	pix, stride := s.Pix(), s.Stride()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		copy(pix[y*stride+4*r.Min.X:], row)
	}
}

// TestPattern draws eight vertical color bars over the top three quarters
// and a horizontal gray ramp below: what `gpu test` shows, and what
// check-virt looks for in QEMU's screendump.
func TestPattern(s Surface) {
	b := s.Bounds()
	split := b.Min.Y + 3*b.Dy()/4
	for i, c := range Bars {
		x0 := b.Min.X + i*b.Dx()/len(Bars)
		x1 := b.Min.X + (i+1)*b.Dx()/len(Bars)
		Fill(s, image.Rect(x0, b.Min.Y, x1, split), c)
	}
	for x := b.Min.X; x < b.Max.X; x++ {
		v := uint8(255 * (x - b.Min.X) / max(1, b.Dx()-1))
		Fill(s, image.Rect(x, split, x+1, b.Max.Y), color.RGBA{v, v, v, 0xff})
	}
}

// Bars are the test pattern's colors, left to right.
var Bars = []color.RGBA{
	{0xff, 0xff, 0xff, 0xff}, {0xff, 0xff, 0x00, 0xff}, {0x00, 0xff, 0xff, 0xff}, {0x00, 0xff, 0x00, 0xff},
	{0xff, 0x00, 0xff, 0xff}, {0xff, 0x00, 0x00, 0xff}, {0x00, 0x00, 0xff, 0xff}, {0x00, 0x00, 0x00, 0xff},
}
