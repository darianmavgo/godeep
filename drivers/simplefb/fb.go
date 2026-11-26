// Package simplefb is a text console on a firmware-provided framebuffer: the
// "simple-framebuffer" that m1n1 (and U-Boot, and QEMU's ramfb) leave set up
// and describe in the device tree. No display driver is needed: drawing is
// just writing pixels to memory.
package simplefb

import (
	"fmt"
	"image"
	"image/color"
)

// Format is a device-tree "format" string.
type Format string

const (
	X8R8G8B8    Format = "x8r8g8b8"    // 32 bpp
	X2R10G10B10 Format = "x2r10g10b10" // 32 bpp, 10 bits per channel (Apple XDR displays)
	R5G6B5      Format = "r5g6b5"      // 16 bpp
)

// Framebuffer is linear pixel memory.
type Framebuffer struct {
	Pix           []byte // Stride*Height bytes
	Width, Height int
	Stride        int // bytes per row
	Format        Format
}

func (f *Framebuffer) bpp() int {
	if f.Format == R5G6B5 {
		return 2
	}
	return 4
}

// Validate checks the description before anything is drawn.
func (f *Framebuffer) Validate() error {
	switch f.Format {
	case X8R8G8B8, X2R10G10B10, R5G6B5:
	default:
		return fmt.Errorf("simplefb: unsupported format %q", f.Format)
	}
	if f.Width <= 0 || f.Height <= 0 || f.Stride < f.Width*f.bpp() || len(f.Pix) < f.Stride*f.Height {
		return fmt.Errorf("simplefb: bad geometry %dx%d stride %d (%d bytes)", f.Width, f.Height, f.Stride, len(f.Pix))
	}
	return nil
}

// RGB encodes an 8-bit-per-channel colour in the framebuffer's format.
func (f *Framebuffer) RGB(r, g, b uint8) uint32 {
	switch f.Format {
	case X2R10G10B10:
		e := func(c uint8) uint32 { return uint32(c)<<2 | uint32(c)>>6 }
		return e(r)<<20 | e(g)<<10 | e(b)
	case R5G6B5:
		return uint32(r>>3)<<11 | uint32(g>>2)<<5 | uint32(b>>3)
	}
	return uint32(r)<<16 | uint32(g)<<8 | uint32(b)
}

func (f *Framebuffer) rgbaAt(x, y int) color.RGBA {
	o := y*f.Stride + x*f.bpp()
	switch f.Format {
	case R5G6B5:
		v := uint16(f.Pix[o]) | uint16(f.Pix[o+1])<<8
		return color.RGBA{uint8(v>>11) << 3, uint8(v>>5&0x3f) << 2, uint8(v&0x1f) << 3, 255}
	case X2R10G10B10:
		v := uint32(f.Pix[o]) | uint32(f.Pix[o+1])<<8 | uint32(f.Pix[o+2])<<16 | uint32(f.Pix[o+3])<<24
		return color.RGBA{uint8(v >> 22), uint8(v >> 12), uint8(v >> 2), 255}
	}
	return color.RGBA{f.Pix[o+2], f.Pix[o+1], f.Pix[o], 255}
}

// Image decodes the framebuffer into an RGBA image (e.g. to save a
// screenshot of a memory dump as PNG). The framebuffer must be Valid.
func (f *Framebuffer) Image() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			img.SetRGBA(x, y, f.rgbaAt(x, y))
		}
	}
	return img
}

func (f *Framebuffer) set(x, y int, c uint32) {
	o := y*f.Stride + x*f.bpp()
	if f.bpp() == 2 {
		f.Pix[o], f.Pix[o+1] = byte(c), byte(c>>8)
		return
	}
	f.Pix[o], f.Pix[o+1], f.Pix[o+2], f.Pix[o+3] = byte(c), byte(c>>8), byte(c>>16), byte(c>>24)
}

func (f *Framebuffer) fill(y0, y1 int, c uint32) {
	for y := y0; y < y1; y++ {
		for x := 0; x < f.Width; x++ {
			f.set(x, y, c)
		}
	}
}

func (f *Framebuffer) get(x, y int) uint32 {
	o := y*f.Stride + x*f.bpp()
	if f.bpp() == 2 {
		return uint32(f.Pix[o]) | uint32(f.Pix[o+1])<<8
	}
	return uint32(f.Pix[o]) | uint32(f.Pix[o+1])<<8 | uint32(f.Pix[o+2])<<16 | uint32(f.Pix[o+3])<<24
}
