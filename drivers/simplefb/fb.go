// Package simplefb is a text console on a firmware-provided framebuffer: the
// "simple-framebuffer" that m1n1 (and U-Boot, and QEMU's ramfb) leave set up
// and describe in the device tree. No display driver is needed: drawing is
// just writing pixels to memory.
package simplefb

import (
	"fmt"
	"image"
	"image/color"
	"sync"

	"golang.org/x/image/font/basicfont"
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

// Console renders text with the 7x13 bitmap font from x/image, scaled up
// for high-density screens, and scrolls when the screen is full.
type Console struct {
	mu       sync.Mutex
	fb       *Framebuffer
	scale    int
	cw, ch   int // character cell in pixels
	cols     int
	rows     int
	col, row int
	fg, bg   uint32
}

var face = basicfont.Face7x13

// NewConsole clears the screen and returns a console on it. scale 0 picks one
// so lines stay readable (about 1200 px of width per step).
func NewConsole(fb *Framebuffer, scale int) (*Console, error) {
	if err := fb.Validate(); err != nil {
		return nil, err
	}
	if scale <= 0 {
		scale = max(1, fb.Width/1200)
	}
	c := &Console{fb: fb, scale: scale, cw: face.Advance * scale, ch: face.Height * scale,
		fg: fb.RGB(0xe6, 0xe6, 0xe6), bg: fb.RGB(0x10, 0x14, 0x1c)}
	c.cols, c.rows = fb.Width/c.cw, fb.Height/c.ch
	if c.cols < 1 || c.rows < 1 {
		return nil, fmt.Errorf("simplefb: %dx%d is too small for text", fb.Width, fb.Height)
	}
	fb.fill(0, fb.Height, c.bg)
	return c, nil
}

// Size returns the console size in characters.
func (c *Console) Size() (cols, rows int) { return c.cols, c.rows }

func (c *Console) glyph(r rune, col, row int) {
	idx := -1
	for _, rg := range face.Ranges {
		if r >= rg.Low && r < rg.High {
			idx = rg.Offset + int(r-rg.Low)
			break
		}
	}
	if idx < 0 {
		idx = 0 // the font's placeholder glyph
	}
	x0, y0 := col*c.cw, row*c.ch
	for gy := 0; gy < face.Height; gy++ {
		for gx := 0; gx < face.Advance; gx++ {
			on := gx < face.Width && face.Mask.At(gx, idx*face.Height+gy) != nil && alpha(face, gx, idx*face.Height+gy)
			col := c.bg
			if on {
				col = c.fg
			}
			for sy := 0; sy < c.scale; sy++ {
				for sx := 0; sx < c.scale; sx++ {
					c.fb.set(x0+gx*c.scale+sx, y0+gy*c.scale+sy, col)
				}
			}
		}
	}
}

func alpha(f *basicfont.Face, x, y int) bool {
	_, _, _, a := f.Mask.At(x, y).RGBA()
	return a > 0x7fff
}

func (c *Console) newline() {
	c.col = 0
	if c.row++; c.row < c.rows {
		return
	}
	// scroll up one text row
	c.row = c.rows - 1
	line := c.ch * c.fb.Stride
	used := c.rows * c.ch * c.fb.Stride
	copy(c.fb.Pix[:used-line], c.fb.Pix[line:used])
	c.fb.fill((c.rows-1)*c.ch, c.rows*c.ch, c.bg)
}

// Write draws text. It understands \n, \r, \b and \t; other control
// characters are ignored.
func (c *Console) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range string(p) {
		switch b {
		case '\n':
			c.newline()
		case '\r':
			c.col = 0
		case '\b':
			if c.col > 0 {
				c.col--
			}
		case '\t':
			for c.col%8 != 7 && c.col < c.cols-1 {
				c.glyph(' ', c.col, c.row)
				c.col++
			}
			c.glyph(' ', c.col, c.row)
			c.col++
		default:
			if b < 0x20 {
				continue
			}
			if c.col >= c.cols {
				c.newline()
			}
			c.glyph(b, c.col, c.row)
			c.col++
		}
	}
	return len(p), nil
}
