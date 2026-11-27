package simplefb

import (
	"strconv"
	"strings"
)

// The console understands the few ANSI (ECMA-48) sequences a full-screen
// program needs, so the editor works on the framebuffer as on a serial or
// SSH terminal:
//
//	ESC [ row ; col H   (or f) move the cursor (1-based; default 1;1)
//	ESC [ n A/B/C/D     cursor up/down/right/left
//	ESC [ 2 J           clear the screen (0 J: from the cursor on)
//	ESC [ K             erase to the end of the line (2 K: the whole line)
//	ESC [ 7 m, 0 m, m   reverse video on, attributes off (27 m: reverse off)
//	ESC [ ? 25 h / l    show / hide the text cursor (hidden by default)
//
// Anything else is swallowed whole.

// revBit marks a reverse-video cell in the stored text.
const revBit = 1 << 30

// escape feeds one character of an escape sequence (the first is ESC).
func (c *Console) escape(r rune) {
	switch c.esc {
	case 0:
		c.esc, c.escParams = 1, c.escParams[:0]
	case 1:
		if r == '[' {
			c.esc = 2
		} else {
			c.esc = 0 // ESC x: two-character sequence, ignored
		}
	case 2:
		if r >= 0x40 && r <= 0x7e {
			c.esc = 0
			c.csi(string(c.escParams), byte(r))
		} else if len(c.escParams) < 32 {
			c.escParams = append(c.escParams, byte(r))
		}
	}
}

func (c *Console) csi(params string, final byte) {
	private := strings.HasPrefix(params, "?")
	params = strings.TrimPrefix(params, "?")
	var n []int
	for _, f := range strings.Split(params, ";") {
		v, err := strconv.Atoi(f)
		if err != nil {
			v = 0
		}
		n = append(n, v)
	}
	arg := func(i, def int) int {
		if i < len(n) && n[i] > 0 {
			return n[i]
		}
		return def
	}
	if private {
		if params == "25" && (final == 'h' || final == 'l') {
			c.cursorOn = final == 'h'
		}
		return
	}
	switch final {
	case 'H', 'f':
		c.row = min(arg(0, 1), c.rows) - 1
		c.col = min(arg(1, 1), c.cols) - 1
	case 'A':
		c.row = max(c.row-arg(0, 1), 0)
	case 'B':
		c.row = min(c.row+arg(0, 1), c.rows-1)
	case 'C':
		c.col = min(c.col+arg(0, 1), c.cols-1)
	case 'D':
		c.col = max(min(c.col, c.cols-1)-arg(0, 1), 0)
	case 'J':
		from := 0
		switch n[0] {
		case 0:
			c.clearCells(c.row, min(c.col, c.cols), c.cols)
			from = c.row + 1
		case 2, 3:
		default:
			return
		}
		for row := from; row < c.rows; row++ {
			c.clearCells(row, 0, c.cols)
		}
	case 'K':
		switch n[0] {
		case 0:
			c.clearCells(c.row, min(c.col, c.cols), c.cols)
		case 1:
			c.clearCells(c.row, 0, min(c.col+1, c.cols))
		case 2:
			c.clearCells(c.row, 0, c.cols)
		}
	case 'm':
		for _, v := range n {
			switch v {
			case 0:
				c.reverse = false
			case 7:
				c.reverse = true
			case 27:
				c.reverse = false
			}
		}
	}
}

// clearCells blanks columns c0..c1-1 of a screen row.
func (c *Console) clearCells(row, c0, c1 int) {
	line := c.lines[c.top()+row]
	for col := c0; col < c1; col++ {
		if line[col] != ' ' {
			line[col] = ' '
			c.cell(col, row)
		}
	}
}

// The text cursor is drawn as the cell under it in reverse, after a
// Write, and taken away before the next one.
func (c *Console) showCursor() {
	if !c.cursorOn || c.view != 0 {
		return
	}
	col, row := min(c.col, c.cols-1), c.row
	r := c.lines[c.top()+row][col]
	c.glyph(r^revBit, col, row, c.fg, c.bg)
	c.curShown, c.curCol, c.curRow = true, col, row
}

func (c *Console) hideCursor() {
	if c.curShown {
		c.curShown = false
		c.cell(c.curCol, c.curRow)
	}
}
