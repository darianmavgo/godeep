// Package vt is a terminal emulator's core: it parses what programs write
// (text, UTF-8, the xterm subset of ECMA-48 escapes full-screen programs
// use) into a grid of cells, and keeps scrollback. It draws nothing:
// a renderer (programs/sh's desktop terminals) reads the cells through
// View and redraws the rows marked dirty.
//
// Supported: CR, LF (also returns the carriage, as godeep's shell writes
// bare "\n"), BS, TAB, BEL (ignored); ESC 7/8 (save/restore cursor),
// D/E/M (index, next line, reverse index), c (reset); CSI A-G, H/f, d,
// J, K, L, M, P, @, X, S, T, m (SGR: bold, underline, reverse, 16 and 256
// colors, 24-bit color), r (scroll region), s/u, ?25 h/l (cursor),
// ?1049/?1047/?47 h/l (alternate screen), ?7 h/l (autowrap); OSC 0/2
// (title). Anything else is consumed and ignored.
//
// A Term is safe for concurrent use: the shell writes from its goroutines
// while the renderer reads.
package vt

import (
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Color is a cell color: Default, an indexed color (0-255: the 16 ANSI
// colors, the 6x6x6 cube, the gray ramp), or 24-bit RGB (RGB flag).
type Color uint32

const (
	Default Color = 1 << 31
	RGBFlag Color = 1 << 30
)

// RGB makes a 24-bit color.
func RGB(r, g, b uint8) Color { return RGBFlag | Color(r)<<16 | Color(g)<<8 | Color(b) }

// Attribute bits.
const (
	Bold      = 1 << 0
	Underline = 1 << 1
	Reverse   = 1 << 2
	Italic    = 1 << 3
)

// Cell is one character position.
type Cell struct {
	R      rune // 0 or ' ' for blank
	FG, BG Color
	Attr   uint8
}

// History is how many lines scrolled off the top a Term keeps.
const History = 2000

// Term is a terminal screen.
type Term struct {
	mu         sync.Mutex
	cols, rows int
	main, alt  [][]Cell // the screens; cur points at one
	cur        *[][]Cell
	hist       [][]Cell // scrollback of the main screen
	x, y       int
	wrapNext   bool // xterm's deferred wrap: the next printable goes to the next line
	pen        Cell // attributes for new text
	savedX     int
	savedY     int
	savedPen   Cell
	top, bot   int // scroll region, inclusive rows
	cursorOn   bool
	autowrap   bool
	title      string
	dirty      []bool
	changed    chan struct{}

	// parser
	state  int
	params []byte
	inter  byte // '?' or '>' private marker
	osc    []byte
	utf    []byte // a partial UTF-8 sequence
}

const (
	stGround = iota
	stEsc
	stCSI
	stOSC
	stOSCEsc  // ESC inside an OSC (ST is ESC \)
	stCharset // ESC ( X etc.: one more byte
)

// New returns a cols x rows terminal.
func New(cols, rows int) *Term {
	t := &Term{cols: max(1, cols), rows: max(1, rows), changed: make(chan struct{}, 1), cursorOn: true, autowrap: true}
	t.pen = Cell{R: ' ', FG: Default, BG: Default}
	t.main = t.blankScreen()
	t.alt = t.blankScreen()
	t.cur = &t.main
	t.top, t.bot = 0, t.rows-1
	t.dirty = make([]bool, t.rows)
	t.allDirty()
	return t
}

func (t *Term) blankLine() []Cell {
	l := make([]Cell, t.cols)
	for i := range l {
		l[i] = Cell{R: ' ', FG: Default, BG: t.pen.BG}
	}
	return l
}

func (t *Term) blankScreen() [][]Cell {
	s := make([][]Cell, t.rows)
	for i := range s {
		s[i] = t.blankLine()
	}
	return s
}

func (t *Term) allDirty() {
	for i := range t.dirty {
		t.dirty[i] = true
	}
}

func (t *Term) notify() {
	select {
	case t.changed <- struct{}{}:
	default:
	}
}

// Changed receives a value after writes change the screen.
func (t *Term) Changed() <-chan struct{} { return t.changed }

// Size is the terminal's size in cells.
func (t *Term) Size() (cols, rows int) { return t.cols, t.rows }

// Title is the last title set with OSC 0 or 2 ("" if none).
func (t *Term) Title() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.title
}

// Screen describes what to draw.
type Screen struct {
	Rows     [][]Cell // the visible rows (the live screen, or scrollback when scrolled)
	Dirty    []bool   // rows changed since the last View
	CursorX  int
	CursorY  int
	CursorOn bool // the cursor is visible (and the view is the live screen)
}

// View calls f with the screen scrolled back by back lines (0: live), with
// the terminal locked: f must not keep the slices. It clears the dirty
// marks. Scrolling the view marks every row dirty.
func (t *Term) View(back int, f func(Screen)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	back = max(0, min(back, t.scrollbackLen()))
	rows := *t.cur
	if back > 0 {
		h := t.hist[len(t.hist)-back:]
		rows = append(append([][]Cell(nil), h...), (*t.cur)[:t.rows-min(back, t.rows)]...)
		rows = rows[:t.rows]
	}
	f(Screen{Rows: rows, Dirty: t.dirty, CursorX: min(t.x, t.cols-1), CursorY: t.y, CursorOn: t.cursorOn && back == 0})
	for i := range t.dirty {
		t.dirty[i] = false
	}
}

// Scrollback is how many lines of history the view can go back.
func (t *Term) Scrollback() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scrollbackLen()
}

func (t *Term) scrollbackLen() int {
	if t.cur != &t.main {
		return 0
	}
	return len(t.hist)
}

// Invalidate marks every row dirty (the renderer lost its pixels, or the
// view scrolled).
func (t *Term) Invalidate() {
	t.mu.Lock()
	t.allDirty()
	t.mu.Unlock()
	t.notify()
}

// Text returns the live screen as text, trailing blanks trimmed (tests,
// copy).
func (t *Term) Text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	for _, row := range *t.cur {
		var l []rune
		for _, c := range row {
			r := c.R
			if r == 0 {
				r = ' '
			}
			l = append(l, r)
		}
		b.WriteString(strings.TrimRight(string(l), " "))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// Write parses p.
func (t *Term) Write(p []byte) (int, error) {
	t.mu.Lock()
	for _, b := range p {
		t.byte(b)
	}
	t.mu.Unlock()
	t.notify()
	return len(p), nil
}

func (t *Term) byte(b byte) {
	switch t.state {
	case stEsc:
		t.escape(b)
		return
	case stCSI:
		switch {
		case b >= 0x30 && b <= 0x3f:
			if (b == '?' || b == '>' || b == '=') && len(t.params) == 0 && t.inter == 0 {
				t.inter = b
			} else {
				t.params = append(t.params, b)
			}
		case b >= 0x20 && b <= 0x2f: // intermediates: ignored
		case b >= 0x40 && b <= 0x7e:
			t.state = stGround
			t.csi(b)
		case b == 0x1b:
			t.state = stEsc
		case b < 0x20:
			t.control(b) // C0 inside a sequence is executed
		default:
			t.state = stGround
		}
		return
	case stOSC:
		switch b {
		case 0x07:
			t.endOSC()
		case 0x1b:
			t.state = stOSCEsc
		default:
			if len(t.osc) < 512 {
				t.osc = append(t.osc, b)
			}
		}
		return
	case stOSCEsc:
		t.endOSC() // ESC \ (or anything): the OSC ends
		if b != '\\' {
			t.byte(b)
		}
		return
	case stCharset:
		t.state = stGround
		return
	}
	// ground
	if len(t.utf) > 0 {
		t.utf = append(t.utf, b)
		if utf8.FullRune(t.utf) {
			r, _ := utf8.DecodeRune(t.utf)
			t.utf = t.utf[:0]
			t.print(r)
		} else if len(t.utf) >= utf8.UTFMax {
			t.utf = t.utf[:0]
			t.print(utf8.RuneError)
		}
		return
	}
	switch {
	case b == 0x1b:
		t.state = stEsc
	case b < 0x20 || b == 0x7f:
		t.control(b)
	case b < 0x80:
		t.print(rune(b))
	default:
		t.utf = append(t.utf[:0], b)
	}
}

func (t *Term) control(b byte) {
	switch b {
	case '\r':
		t.x, t.wrapNext = 0, false
	case '\n', 0x0b, 0x0c:
		t.x, t.wrapNext = 0, false
		t.lineFeed()
	case '\b':
		if t.wrapNext {
			t.wrapNext = false
		} else if t.x > 0 {
			t.x--
		}
	case '\t':
		t.x = min(t.cols-1, (t.x/8+1)*8)
		t.wrapNext = false
	}
}

func (t *Term) escape(b byte) {
	t.state = stGround
	switch b {
	case '[':
		t.state, t.params, t.inter = stCSI, t.params[:0], 0
	case ']':
		t.state, t.osc = stOSC, t.osc[:0]
	case '(', ')', '*', '+':
		t.state = stCharset
	case '7':
		t.savedX, t.savedY, t.savedPen = t.x, t.y, t.pen
	case '8':
		t.x, t.y, t.pen, t.wrapNext = t.savedX, t.savedY, t.savedPen, false
	case 'D':
		t.lineFeed()
	case 'E':
		t.x = 0
		t.lineFeed()
	case 'M':
		t.reverseIndex()
	case 'c':
		t.reset()
	}
}

func (t *Term) endOSC() {
	t.state = stGround
	s := string(t.osc)
	if n, rest, ok := strings.Cut(s, ";"); ok && (n == "0" || n == "2") {
		t.title = rest
	}
}

func (t *Term) reset() {
	t.pen = Cell{R: ' ', FG: Default, BG: Default}
	t.main, t.alt = t.blankScreen(), t.blankScreen()
	t.cur = &t.main
	t.hist = nil
	t.x, t.y, t.wrapNext = 0, 0, false
	t.top, t.bot = 0, t.rows-1
	t.cursorOn, t.autowrap = true, true
	t.allDirty()
}

func (t *Term) print(r rune) {
	if t.wrapNext && t.autowrap {
		t.x = 0
		t.lineFeed()
	}
	t.wrapNext = false
	c := t.pen
	c.R = r
	(*t.cur)[t.y][t.x] = c
	t.dirty[t.y] = true
	if t.x == t.cols-1 {
		t.wrapNext = true
	} else {
		t.x++
	}
}

// lineFeed moves down, scrolling the region at its bottom.
func (t *Term) lineFeed() {
	if t.y == t.bot {
		t.scrollUp(1, true)
	} else if t.y < t.rows-1 {
		t.y++
	}
}

func (t *Term) reverseIndex() {
	if t.y == t.top {
		t.scrollDown(1)
	} else if t.y > 0 {
		t.y--
	}
}

// scrollUp moves the region's lines up n. With history, lines leaving the
// top of a region that starts at the top of the main screen go to the
// scrollback (a line feed); CSI S and M don't keep them.
func (t *Term) scrollUp(n int, history bool) {
	s := *t.cur
	n = min(n, t.bot-t.top+1)
	for i := 0; i < n; i++ {
		gone := s[t.top]
		if history && t.cur == &t.main && t.top == 0 {
			t.hist = append(t.hist, gone)
			if len(t.hist) > History {
				t.hist = t.hist[len(t.hist)-History:]
			}
		}
		copy(s[t.top:t.bot], s[t.top+1:t.bot+1])
		s[t.bot] = t.blankLine()
	}
	for i := t.top; i <= t.bot; i++ {
		t.dirty[i] = true
	}
}

func (t *Term) scrollDown(n int) {
	s := *t.cur
	n = min(n, t.bot-t.top+1)
	for i := 0; i < n; i++ {
		copy(s[t.top+1:t.bot+1], s[t.top:t.bot])
		s[t.top] = t.blankLine()
	}
	for i := t.top; i <= t.bot; i++ {
		t.dirty[i] = true
	}
}

// param returns parameter i (default def when missing or 0).
func param(ps []int, i, def int) int {
	if i < len(ps) && ps[i] > 0 {
		return ps[i]
	}
	return def
}

func parseParams(b []byte) []int {
	if len(b) == 0 {
		return nil
	}
	var out []int
	for _, s := range strings.Split(string(b), ";") {
		if i := strings.IndexByte(s, ':'); i >= 0 { // 38:2:r:g:b subparameters: take them as separate
			for _, sub := range strings.Split(s, ":") {
				v, _ := strconv.Atoi(sub)
				out = append(out, v)
			}
			continue
		}
		v, _ := strconv.Atoi(s)
		out = append(out, min(v, 65535))
	}
	return out
}

func (t *Term) csi(final byte) {
	ps := parseParams(t.params)
	if t.inter == '?' {
		on := final == 'h'
		if final != 'h' && final != 'l' {
			return
		}
		for _, p := range ps {
			switch p {
			case 25:
				t.cursorOn = on
				t.dirty[t.y] = true
			case 7:
				t.autowrap = on
			case 47, 1047, 1049:
				t.altScreen(on, p == 1049)
			}
		}
		return
	}
	if t.inter != 0 {
		return // > and = private sequences: ignored
	}
	t.wrapNext = false
	n := param(ps, 0, 1)
	switch final {
	case 'A':
		t.y = max(t.y-n, 0)
		if t.y < t.top && t.y+n >= t.top {
			t.y = t.top
		}
	case 'B':
		t.y = min(t.y+n, t.rows-1)
	case 'C':
		t.x = min(t.x+n, t.cols-1)
	case 'D':
		t.x = max(t.x-n, 0)
	case 'E':
		t.x, t.y = 0, min(t.y+n, t.rows-1)
	case 'F':
		t.x, t.y = 0, max(t.y-n, 0)
	case 'G':
		t.x = min(n, t.cols) - 1
	case 'd':
		t.y = min(n, t.rows) - 1
	case 'H', 'f':
		t.y = min(param(ps, 0, 1), t.rows) - 1
		t.x = min(param(ps, 1, 1), t.cols) - 1
	case 'J':
		t.eraseDisplay(param(ps, 0, 0))
	case 'K':
		t.eraseLine(param(ps, 0, 0))
	case 'L':
		if t.y >= t.top && t.y <= t.bot {
			saved := t.top
			t.top = t.y
			t.scrollDown(n)
			t.top = saved
		}
	case 'M':
		if t.y >= t.top && t.y <= t.bot {
			saved := t.top
			t.top = t.y
			t.scrollUp(n, false)
			t.top = saved
		}
	case 'P':
		row := (*t.cur)[t.y]
		n = min(n, t.cols-t.x)
		copy(row[t.x:], row[t.x+n:])
		for i := t.cols - n; i < t.cols; i++ {
			row[i] = t.blankCell()
		}
		t.dirty[t.y] = true
	case '@':
		row := (*t.cur)[t.y]
		n = min(n, t.cols-t.x)
		copy(row[t.x+n:], row[t.x:t.cols-n])
		for i := t.x; i < t.x+n; i++ {
			row[i] = t.blankCell()
		}
		t.dirty[t.y] = true
	case 'X':
		row := (*t.cur)[t.y]
		for i := t.x; i < min(t.x+n, t.cols); i++ {
			row[i] = t.blankCell()
		}
		t.dirty[t.y] = true
	case 'S':
		t.scrollUp(n, false)
	case 'T':
		t.scrollDown(n)
	case 'm':
		t.sgr(ps)
	case 'r':
		top, bot := param(ps, 0, 1)-1, param(ps, 1, t.rows)-1
		if top < bot && bot < t.rows {
			t.top, t.bot = top, bot
			t.x, t.y = 0, 0
		}
	case 's':
		t.savedX, t.savedY, t.savedPen = t.x, t.y, t.pen
	case 'u':
		t.x, t.y, t.pen = t.savedX, t.savedY, t.savedPen
	}
}

// blankCell is an erased cell: the current background (xterm's "bce").
func (t *Term) blankCell() Cell { return Cell{R: ' ', FG: Default, BG: t.pen.BG} }

func (t *Term) eraseLine(mode int) {
	row := (*t.cur)[t.y]
	from, to := t.x, t.cols
	switch mode {
	case 1:
		from, to = 0, min(t.x+1, t.cols)
	case 2:
		from, to = 0, t.cols
	}
	for i := from; i < to; i++ {
		row[i] = t.blankCell()
	}
	t.dirty[t.y] = true
}

func (t *Term) eraseDisplay(mode int) {
	switch mode {
	case 0:
		t.eraseLine(0)
		for y := t.y + 1; y < t.rows; y++ {
			(*t.cur)[y] = t.blankLine()
			t.dirty[y] = true
		}
	case 1:
		t.eraseLine(1)
		for y := 0; y < t.y; y++ {
			(*t.cur)[y] = t.blankLine()
			t.dirty[y] = true
		}
	case 2, 3:
		for y := 0; y < t.rows; y++ {
			(*t.cur)[y] = t.blankLine()
		}
		if mode == 3 {
			t.hist = nil
		}
		t.allDirty()
	}
}

func (t *Term) altScreen(on, saveCursor bool) {
	if on == (t.cur == &t.alt) {
		return
	}
	if on {
		if saveCursor {
			t.savedX, t.savedY, t.savedPen = t.x, t.y, t.pen
		}
		t.alt = t.blankScreen()
		t.cur = &t.alt
	} else {
		t.cur = &t.main
		if saveCursor {
			t.x, t.y, t.pen = t.savedX, t.savedY, t.savedPen
		}
	}
	t.wrapNext = false
	t.allDirty()
}

func (t *Term) sgr(ps []int) {
	if len(ps) == 0 {
		ps = []int{0}
	}
	for i := 0; i < len(ps); i++ {
		p := ps[i]
		switch {
		case p == 0:
			t.pen.FG, t.pen.BG, t.pen.Attr = Default, Default, 0
		case p == 1:
			t.pen.Attr |= Bold
		case p == 3:
			t.pen.Attr |= Italic
		case p == 4:
			t.pen.Attr |= Underline
		case p == 7:
			t.pen.Attr |= Reverse
		case p == 22:
			t.pen.Attr &^= Bold
		case p == 23:
			t.pen.Attr &^= Italic
		case p == 24:
			t.pen.Attr &^= Underline
		case p == 27:
			t.pen.Attr &^= Reverse
		case p >= 30 && p <= 37:
			t.pen.FG = Color(p - 30)
		case p == 39:
			t.pen.FG = Default
		case p >= 40 && p <= 47:
			t.pen.BG = Color(p - 40)
		case p == 49:
			t.pen.BG = Default
		case p >= 90 && p <= 97:
			t.pen.FG = Color(p - 90 + 8)
		case p >= 100 && p <= 107:
			t.pen.BG = Color(p - 100 + 8)
		case p == 38 || p == 48:
			var c Color
			var ok bool
			c, i, ok = extColor(ps, i)
			if ok {
				if p == 38 {
					t.pen.FG = c
				} else {
					t.pen.BG = c
				}
			}
		}
	}
}

// extColor reads 38;5;N or 38;2;R;G;B starting at ps[i] (the 38/48); it
// returns the index of the last parameter used.
func extColor(ps []int, i int) (Color, int, bool) {
	if i+1 >= len(ps) {
		return 0, i, false
	}
	switch ps[i+1] {
	case 5:
		if i+2 < len(ps) {
			return Color(ps[i+2] & 0xff), i + 2, true
		}
	case 2:
		if i+4 < len(ps) {
			return RGB(uint8(ps[i+2]), uint8(ps[i+3]), uint8(ps[i+4])), i + 4, true
		}
	}
	return 0, len(ps), false
}

// Palette resolves a cell color to RGB with a 16-color theme (the
// 6x6x6 cube and the gray ramp are xterm's).
func Palette(c Color, ansi *[16][3]uint8, def [3]uint8) [3]uint8 {
	switch {
	case c == Default:
		return def
	case c&RGBFlag != 0:
		return [3]uint8{uint8(c >> 16), uint8(c >> 8), uint8(c)}
	case c < 16:
		return ansi[c]
	case c < 232:
		i := int(c) - 16
		lv := func(v int) uint8 {
			if v == 0 {
				return 0
			}
			return uint8(55 + 40*v)
		}
		return [3]uint8{lv(i / 36), lv(i / 6 % 6), lv(i % 6)}
	case c < 256:
		g := uint8(8 + 10*(int(c)-232))
		return [3]uint8{g, g, g}
	}
	return def
}
