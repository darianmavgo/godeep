package simplefb

import (
	"fmt"
	"strings"
	"sync"

	"golang.org/x/image/font/basicfont"
)

// History is how many lines scrolled off the top the console keeps.
const History = 2000

// Console renders text with the 7x13 bitmap font from x/image, scaled up
// for high-density screens, and scrolls when the screen is full.
//
// It keeps the text, not just the pixels: the screen plus History lines of
// scrollback. That's what the mouse works on (mouse.go): the view can be
// scrolled back, text selected (drawn inverted) and copied, and a pointer
// drawn over everything, saving the pixels under it.
//
// All methods are safe for concurrent use (output, printk and the mouse
// arrive from different goroutines).
type Console struct {
	mu     sync.Mutex
	fb     *Framebuffer // where drawing goes: the screen, or a shadow while suspended
	screen *Framebuffer // the real framebuffer while suspended (display.go); nil otherwise

	dispOnce     sync.Once
	disp         *display
	scale        int
	cw, ch       int // character cell in pixels
	cols         int
	rows         int
	col, row     int // cursor, on screen
	fg, bg       uint32
	selFg, selBg uint32

	lines [][]rune // scrollback, then the screen (the last rows lines)
	base  int      // absolute number of lines[0]
	view  int      // lines scrolled back from the live screen

	ptrOn        bool // a pointer device has moved: draw the pointer
	ptrShown     bool
	px, py       int      // pointer hotspot, pixels
	under        []uint32 // pixels under the drawn pointer
	sel          bool     // selection active
	drag         bool     // a selection is being dragged out
	selA, selB   pos      // anchor and moving end (absolute line, column)
	ptrX0, ptrY0 int      // where the pointer was drawn

	// ANSI escapes (ansi.go): parser state, the reverse-video attribute
	// for new text, and the text cursor (shown after ESC [ ? 25 h).
	esc       int // 0: text; 1: after ESC; 2: in a CSI sequence
	escParams []byte
	reverse   bool
	cursorOn  bool
	curShown  bool
	curCol    int
	curRow    int
}

// pos is a character position in the whole text: absolute line, column.
type pos struct{ line, col int }

func (a pos) less(b pos) bool { return a.line < b.line || a.line == b.line && a.col < b.col }

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
		fg: fb.RGB(0xe6, 0xe6, 0xe6), bg: fb.RGB(0x10, 0x14, 0x1c),
		selFg: fb.RGB(0x10, 0x14, 0x1c), selBg: fb.RGB(0x9c, 0xc4, 0xff)}
	c.cols, c.rows = fb.Width/c.cw, fb.Height/c.ch
	if c.cols < 1 || c.rows < 1 {
		return nil, fmt.Errorf("simplefb: %dx%d is too small for text", fb.Width, fb.Height)
	}
	for range c.rows {
		c.lines = append(c.lines, c.blank())
	}
	c.px, c.py = fb.Width/2, fb.Height/2
	fb.fill(0, fb.Height, c.bg)
	return c, nil
}

func (c *Console) blank() []rune {
	l := make([]rune, c.cols)
	for i := range l {
		l[i] = ' '
	}
	return l
}

// Size returns the console size in characters.
func (c *Console) Size() (cols, rows int) { return c.cols, c.rows }

// top is the index in lines of the live screen's first row.
func (c *Console) top() int { return len(c.lines) - c.rows }

func (c *Console) glyph(r rune, col, row int, fg, bg uint32) {
	if r&revBit != 0 {
		r &^= revBit
		fg, bg = bg, fg
	}
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
			col := bg
			if on {
				col = fg
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

// cell draws one character of the visible screen from the text.
func (c *Console) cell(col, row int) {
	i := c.top() - c.view + row
	fg, bg := c.fg, c.bg
	if c.selected(pos{c.base + i, col}) {
		fg, bg = c.selFg, c.selBg
	}
	c.glyph(c.lines[i][col], col, row, fg, bg)
}

func (c *Console) drawRow(row int) {
	for col := 0; col < c.cols; col++ {
		c.cell(col, row)
	}
}

func (c *Console) redraw() {
	for row := 0; row < c.rows; row++ {
		c.drawRow(row)
	}
}

// shiftRows moves the text area's pixels up (n > 0) or down by n rows.
func (c *Console) shiftRows(n int) {
	line := c.ch * c.fb.Stride
	used := c.rows * line
	if n > 0 {
		copy(c.fb.Pix[:used-n*line], c.fb.Pix[n*line:used])
	} else {
		copy(c.fb.Pix[-n*line:used], c.fb.Pix[:used+n*line])
	}
}

func (c *Console) newline() {
	c.col = 0
	if c.row++; c.row < c.rows {
		return
	}
	c.row = c.rows - 1
	c.lines = append(c.lines, c.blank())
	if drop := len(c.lines) - c.rows - History; drop > 0 {
		c.lines = append(c.lines[:0:0], c.lines[drop:]...)
		c.base += drop
		if c.sel && min(c.selA.line, c.selB.line) < c.base {
			c.sel = false // scrolled out of history
		}
	}
	c.shiftRows(1)
	c.drawRow(c.rows - 1)
}

// Write draws text. It understands \n, \r, \b and \t; other control
// characters are ignored. Output returns a scrolled-back view to the live
// screen.
func (c *Console) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hidePointer()
	defer c.showPointer()
	c.hideCursor()
	defer c.showCursor()
	if c.view != 0 {
		c.view = 0
		c.redraw()
	}
	put := func(r rune) {
		if c.reverse {
			r |= revBit
		}
		c.lines[c.top()+c.row][c.col] = r
		c.cell(c.col, c.row)
		c.col++
	}
	for _, b := range string(p) {
		if c.esc != 0 || b == 0x1b {
			c.escape(b)
			continue
		}
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
				put(' ')
			}
			put(' ')
		default:
			if b < 0x20 {
				continue
			}
			if c.col >= c.cols {
				c.newline()
			}
			put(b)
		}
	}
	return len(p), nil
}

// ScrollView scrolls the view n lines back into history (n < 0: forward),
// within what's there. It returns the new distance from the live screen.
func (c *Console) ScrollView(n int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := min(max(c.view+n, 0), c.top())
	d := v - c.view
	if d == 0 {
		return v
	}
	c.hidePointer()
	defer c.showPointer()
	c.view = v
	if d >= c.rows || -d >= c.rows {
		c.redraw()
		return v
	}
	// Scrolling back moves the text down: new (older) rows appear on top.
	c.shiftRows(-d)
	if d > 0 {
		for row := 0; row < d; row++ {
			c.drawRow(row)
		}
	} else {
		for row := c.rows + d; row < c.rows; row++ {
			c.drawRow(row)
		}
	}
	return v
}

// ---- pointer ----

// The pointer: an arrow, '#' outline and '.' fill, hotspot top left.
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
	"     #..#",
	"      ##",
}

func (c *Console) ptrScale() int { return max(1, c.scale) }

// hidePointer restores the pixels under the pointer (before drawing text).
func (c *Console) hidePointer() {
	if !c.ptrShown {
		return
	}
	s, i := c.ptrScale(), 0
	for ay, line := range arrow {
		for ax := range line {
			if line[ax] == ' ' {
				continue
			}
			for sy := 0; sy < s; sy++ {
				for sx := 0; sx < s; sx++ {
					x, y := c.ptrX0+ax*s+sx, c.ptrY0+ay*s+sy
					if x < c.fb.Width && y < c.fb.Height {
						c.fb.set(x, y, c.under[i])
					}
					i++
				}
			}
		}
	}
	c.ptrShown = false
}

// showPointer saves the pixels under the pointer and draws it.
func (c *Console) showPointer() {
	if !c.ptrOn || c.ptrShown {
		return
	}
	s := c.ptrScale()
	c.under = c.under[:0]
	black, white := c.fb.RGB(0, 0, 0), c.fb.RGB(0xff, 0xff, 0xff)
	c.ptrX0, c.ptrY0 = c.px, c.py
	for ay, line := range arrow {
		for ax := range line {
			if line[ax] == ' ' {
				continue
			}
			v := white
			if line[ax] == '#' {
				v = black
			}
			for sy := 0; sy < s; sy++ {
				for sx := 0; sx < s; sx++ {
					x, y := c.px+ax*s+sx, c.py+ay*s+sy
					if x < c.fb.Width && y < c.fb.Height {
						c.under = append(c.under, c.fb.get(x, y))
						c.fb.set(x, y, v)
					} else {
						c.under = append(c.under, 0) // keeps hidePointer's indices aligned
					}
				}
			}
		}
	}
	c.ptrShown = true
}

// MovePointer moves the pointer by dx, dy pixels, within the screen, and
// shows it (it stays hidden until a pointing device first moves).
func (c *Console) MovePointer(dx, dy int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hidePointer()
	c.ptrOn = true
	c.px = min(max(c.px+dx, 0), c.fb.Width-1)
	c.py = min(max(c.py+dy, 0), c.fb.Height-1)
	if c.sel && c.dragging() {
		c.extend(c.pointerPos())
	}
	c.showPointer()
}

// PointTo moves the pointer to a position given as a fraction of the
// screen (x, y in 0..max: an absolute pointing device), and shows it.
func (c *Console) PointTo(x, y, max int) {
	if max <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hidePointer()
	c.ptrOn = true
	c.px = min(max0(x)*(c.fb.Width-1)/max, c.fb.Width-1)
	c.py = min(max0(y)*(c.fb.Height-1)/max, c.fb.Height-1)
	if c.sel && c.dragging() {
		c.extend(c.pointerPos())
	}
	c.showPointer()
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// Pointer returns the pointer's position in pixels and whether it's shown.
func (c *Console) Pointer() (x, y int, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.px, c.py, c.ptrOn
}

// pointerPos is the character under the pointer.
func (c *Console) pointerPos() pos {
	row := min(c.py/c.ch, c.rows-1)
	col := min(c.px/c.cw, c.cols-1)
	return pos{c.base + c.top() - c.view + row, col}
}

// ---- selection ----

// dragging is true between SelectStart and SelectEnd.
func (c *Console) dragging() bool { return c.drag }

func (c *Console) selected(p pos) bool {
	if !c.sel {
		return false
	}
	a, b := c.selA, c.selB
	if b.less(a) {
		a, b = b, a
	}
	return !p.less(a) && !b.less(p)
}

// redrawLines redraws the visible rows showing absolute lines l0..l1.
func (c *Console) redrawLines(l0, l1 int) {
	if l1 < l0 {
		l0, l1 = l1, l0
	}
	first := c.base + c.top() - c.view
	for row := max(l0-first, 0); row <= min(l1-first, c.rows-1); row++ {
		c.drawRow(row)
	}
}

// extend moves the selection's end to p, redrawing what changed.
func (c *Console) extend(p pos) {
	old := c.selB
	c.selB = p
	c.redrawLines(min(old.line, p.line), max(old.line, p.line))
}

// SelectStart starts a selection at the pointer (dropping any old one).
func (c *Console) SelectStart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hidePointer()
	defer c.showPointer()
	c.clearSelection()
	p := c.pointerPos()
	c.sel, c.drag, c.selA, c.selB = true, true, p, p
	c.redrawLines(p.line, p.line)
}

// SelectEnd finishes a selection and returns its text. A click without
// movement selects nothing (and clears).
func (c *Console) SelectEnd() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hidePointer()
	defer c.showPointer()
	c.drag = false
	if !c.sel {
		return ""
	}
	if c.selA == c.selB {
		c.clearSelection()
		return ""
	}
	return c.selectionText()
}

// ClearSelection removes the selection's highlight.
func (c *Console) ClearSelection() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hidePointer()
	defer c.showPointer()
	c.clearSelection()
}

func (c *Console) clearSelection() {
	if !c.sel {
		return
	}
	c.sel, c.drag = false, false
	c.redrawLines(c.selA.line, c.selB.line)
}

// selectionText is the selected text: lines without trailing blanks,
// joined with newlines.
func (c *Console) selectionText() string {
	a, b := c.selA, c.selB
	if b.less(a) {
		a, b = b, a
	}
	var out []string
	for l := max(a.line, c.base); l <= b.line && l-c.base < len(c.lines); l++ {
		line := c.lines[l-c.base]
		c0, c1 := 0, c.cols-1
		if l == a.line {
			c0 = a.col
		}
		if l == b.line {
			c1 = b.col
		}
		seg := make([]rune, 0, c1+1-c0)
		for _, r := range line[c0 : c1+1] {
			seg = append(seg, r&^revBit)
		}
		out = append(out, strings.TrimRight(string(seg), " "))
	}
	return strings.Join(out, "\n")
}

// Status is the lsdev view: size, history, view, pointer, selection.
func (c *Console) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := fmt.Sprintf("%dx%d text (%dx%d px), %d lines of history", c.cols, c.rows, c.fb.Width, c.fb.Height, c.top())
	if c.view > 0 {
		s += fmt.Sprintf(", viewing %d back", c.view)
	}
	if c.ptrOn {
		s += fmt.Sprintf(", pointer at %d,%d", c.px, c.py)
	} else {
		s += ", no pointer yet"
	}
	if c.sel {
		s += fmt.Sprintf(", %d characters selected", len(c.selectionText()))
	}
	if c.screen != nil {
		s += ", screen lent to a compositor"
	}
	return s
}
