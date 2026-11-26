package simplefb

// Early draws on the firmware framebuffer before the console exists: boot
// stage markers, and a fault screen when the CPU takes an exception that
// nothing else can report. On the Mac there's no serial port, so these are
// the only clues when godeep stops before its console comes up.
//
// Everything here is allocation-free and //go:nosplit: it runs in Hwinit0
// (before the heap) and from the early exception vector, on a small stack
// that the runtime's stack checks don't know about.
type Early struct {
	Pix           []byte
	Width, Height int
	Stride        int
	bpp           int
	x2r10         bool
}

// NewEarly validates a framebuffer description (as fdt.ScanEarly returns it).
//
//go:nosplit
func NewEarly(pix []byte, width, height, stride int, format []byte) (e Early, ok bool) {
	switch string(format) {
	case string(X8R8G8B8):
		e.bpp = 4
	case string(X2R10G10B10):
		e.bpp, e.x2r10 = 4, true
	case string(R5G6B5):
		e.bpp = 2
	default:
		return Early{}, false
	}
	if width <= 0 || height <= 0 || stride < width*e.bpp || len(pix) < stride*height {
		return Early{}, false
	}
	e.Pix, e.Width, e.Height, e.Stride = pix, width, height, stride
	return e, true
}

// Valid reports whether e describes a usable framebuffer.
//
//go:nosplit
func (e *Early) Valid() bool { return e.bpp != 0 }

//go:nosplit
func (e *Early) rgb(r, g, b uint8) uint32 {
	switch {
	case e.x2r10:
		return (uint32(r)<<2|uint32(r)>>6)<<20 | (uint32(g)<<2|uint32(g)>>6)<<10 | (uint32(b)<<2 | uint32(b)>>6)
	case e.bpp == 2:
		return uint32(r>>3)<<11 | uint32(g>>2)<<5 | uint32(b>>3)
	}
	return uint32(r)<<16 | uint32(g)<<8 | uint32(b)
}

// Rect fills [x0,x1) x [y0,y1), clipped to the screen.
//
//go:nosplit
func (e *Early) Rect(x0, y0, x1, y1 int, c uint32) {
	if !e.Valid() {
		return
	}
	x0, y0 = max(x0, 0), max(y0, 0)
	x1, y1 = min(x1, e.Width), min(y1, e.Height)
	for y := y0; y < y1; y++ {
		row := y * e.Stride
		for x := x0; x < x1; x++ {
			o := row + x*e.bpp
			if e.bpp == 2 {
				e.Pix[o], e.Pix[o+1] = byte(c), byte(c>>8)
			} else {
				e.Pix[o], e.Pix[o+1], e.Pix[o+2], e.Pix[o+3] = byte(c), byte(c>>8), byte(c>>16), byte(c>>24)
			}
		}
	}
}

// Stage colours: stage n is drawn as square n in markColors[n-1].
var markColors = [...][3]uint8{
	{0x30, 0x80, 0xff}, // 1 blue
	{0x20, 0xc0, 0x60}, // 2 green
	{0xff, 0xc0, 0x20}, // 3 yellow
	{0xc0, 0x50, 0xff}, // 4 purple
	{0x20, 0xd0, 0xd0}, // 5 cyan
	{0xff, 0x80, 0x30}, // 6 orange
	{0xff, 0xff, 0xff}, // 7 white
	{0xff, 0x60, 0xa0}, // 8 pink
}

// MarkSize is the side of a stage square in pixels.
//
//go:nosplit
func (e *Early) MarkSize() int { return max(8, e.Width/48) }

// MarkOrigin is the top-left corner of stage n's square (n >= 1).
//
//go:nosplit
func (e *Early) MarkOrigin(n int) (x, y int) {
	s := e.MarkSize()
	return s/2 + (n-1)*(s+s/2), s / 2
}

// Mark draws boot stage n (1-based) as a coloured square, with its number,
// in a row along the top of the screen. A row of squares 1..n on a frozen
// screen means stage n was the last one reached.
//
//go:nosplit
func (e *Early) Mark(n int) {
	if !e.Valid() || n < 1 {
		return
	}
	col := markColors[(n-1)%len(markColors)]
	s := e.MarkSize()
	x, y := e.MarkOrigin(n)
	e.Rect(x, y, x+s, y+s, e.rgb(col[0], col[1], col[2]))
	if sc := s / 10; sc >= 1 && n < 10 {
		e.glyph(x+(s-5*sc)/2, y+(s-7*sc)/2, sc, byte('0'+n), e.rgb(0, 0, 0))
	}
}

// Fault fills the screen red and writes the exception registers on it.
//
//go:nosplit
func (e *Early) Fault(stage int, esr, elr, far uint64) {
	if !e.Valid() {
		return
	}
	e.Rect(0, 0, e.Width, e.Height, e.rgb(0x90, 0x10, 0x10))
	sc := max(2, e.Width/320)
	white := e.rgb(0xff, 0xff, 0xff)
	x, y, line := 8*sc, 8*sc, 10*sc
	e.Text(x, y, sc, "GODEEP EARLY FAULT", white)
	y += 2 * line
	e.Text(x, y, sc, "STAGE ", white)
	e.Hex(x+6*6*sc, y, sc, uint64(stage), 2, white)
	y += line
	e.Text(x, y, sc, "ESR ", white)
	e.Hex(x+4*6*sc, y, sc, esr, 16, white)
	y += line
	e.Text(x, y, sc, "ELR ", white)
	e.Hex(x+4*6*sc, y, sc, elr, 16, white)
	y += line
	e.Text(x, y, sc, "FAR ", white)
	e.Hex(x+4*6*sc, y, sc, far, 16, white)
	y += 2 * line
	e.Text(x, y, sc, "HOLD POWER TO RESTART", white)
}

// Text draws s with the built-in 5x7 font (digits, A-Z subset, a few
// symbols; anything else is blank), each pixel scaled to sc x sc.
//
//go:nosplit
func (e *Early) Text(x, y, sc int, s string, c uint32) {
	for i := 0; i < len(s); i++ {
		e.glyph(x+i*6*sc, y, sc, s[i], c)
	}
}

// Hex draws the low `digits` hex digits of v.
//
//go:nosplit
func (e *Early) Hex(x, y, sc int, v uint64, digits int, c uint32) {
	for i := 0; i < digits; i++ {
		d := byte(v >> (4 * (digits - 1 - i)) & 0xf)
		ch := '0' + d
		if d >= 10 {
			ch = 'A' + d - 10
		}
		e.glyph(x+i*6*sc, y, sc, ch, c)
	}
}

//go:nosplit
func (e *Early) glyph(x, y, sc int, ch byte, c uint32) {
	if ch >= 128 {
		return
	}
	g := &font5x7[ch]
	for row := 0; row < 7; row++ {
		bits := g[row]
		for col := 0; col < 5; col++ {
			if bits&(0x10>>col) != 0 {
				e.Rect(x+col*sc, y+row*sc, x+(col+1)*sc, y+(row+1)*sc, c)
			}
		}
	}
}

// font5x7: each glyph is 7 rows of 5 bits, the leftmost pixel in bit 4.
var font5x7 = [128][7]uint8{
	'0': {0x0e, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0e},
	'1': {0x04, 0x0c, 0x04, 0x04, 0x04, 0x04, 0x0e},
	'2': {0x0e, 0x11, 0x01, 0x02, 0x04, 0x08, 0x1f},
	'3': {0x1f, 0x02, 0x04, 0x02, 0x01, 0x11, 0x0e},
	'4': {0x02, 0x06, 0x0a, 0x12, 0x1f, 0x02, 0x02},
	'5': {0x1f, 0x10, 0x1e, 0x01, 0x01, 0x11, 0x0e},
	'6': {0x06, 0x08, 0x10, 0x1e, 0x11, 0x11, 0x0e},
	'7': {0x1f, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08},
	'8': {0x0e, 0x11, 0x11, 0x0e, 0x11, 0x11, 0x0e},
	'9': {0x0e, 0x11, 0x11, 0x0f, 0x01, 0x02, 0x0c},
	'A': {0x0e, 0x11, 0x11, 0x1f, 0x11, 0x11, 0x11},
	'B': {0x1e, 0x11, 0x11, 0x1e, 0x11, 0x11, 0x1e},
	'C': {0x0e, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0e},
	'D': {0x1c, 0x12, 0x11, 0x11, 0x11, 0x12, 0x1c},
	'E': {0x1f, 0x10, 0x10, 0x1e, 0x10, 0x10, 0x1f},
	'F': {0x1f, 0x10, 0x10, 0x1e, 0x10, 0x10, 0x10},
	'G': {0x0e, 0x11, 0x10, 0x17, 0x11, 0x11, 0x0f},
	'H': {0x11, 0x11, 0x11, 0x1f, 0x11, 0x11, 0x11},
	'I': {0x0e, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0e},
	'L': {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1f},
	'M': {0x11, 0x1b, 0x15, 0x15, 0x11, 0x11, 0x11},
	'N': {0x11, 0x11, 0x19, 0x15, 0x13, 0x11, 0x11},
	'O': {0x0e, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0e},
	'P': {0x1e, 0x11, 0x11, 0x1e, 0x10, 0x10, 0x10},
	'R': {0x1e, 0x11, 0x11, 0x1e, 0x14, 0x12, 0x11},
	'S': {0x0f, 0x10, 0x10, 0x0e, 0x01, 0x01, 0x1e},
	'T': {0x1f, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
	'U': {0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0e},
	'V': {0x11, 0x11, 0x11, 0x11, 0x11, 0x0a, 0x04},
	'W': {0x11, 0x11, 0x11, 0x15, 0x15, 0x15, 0x0a},
	'X': {0x11, 0x11, 0x0a, 0x04, 0x0a, 0x11, 0x11},
	'Y': {0x11, 0x11, 0x0a, 0x04, 0x04, 0x04, 0x04},
	':': {0x00, 0x0c, 0x0c, 0x00, 0x0c, 0x0c, 0x00},
	'-': {0x00, 0x00, 0x00, 0x1f, 0x00, 0x00, 0x00},
	'=': {0x00, 0x00, 0x1f, 0x00, 0x1f, 0x00, 0x00},
}
