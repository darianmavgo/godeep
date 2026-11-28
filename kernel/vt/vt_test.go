package vt

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func write(t *Term, s string) { t.Write([]byte(s)) }

func cell(t *Term, x, y int) Cell {
	var c Cell
	t.View(0, func(s Screen) { c = s.Rows[y][x] })
	return c
}

func cursor(t *Term) (int, int) {
	var x, y int
	t.View(0, func(s Screen) { x, y = s.CursorX, s.CursorY })
	return x, y
}

func TestText(t *testing.T) {
	vt := New(10, 3)
	write(vt, "hello\nworld")
	if got := vt.Text(); got != "hello\nworld" {
		t.Fatalf("%q", got)
	}
	// \n returns the carriage (godeep's shell writes bare \n)
	if x, y := cursor(vt); x != 5 || y != 1 {
		t.Fatalf("cursor %d,%d", x, y)
	}
	write(vt, "\r\nabc\b\bX")
	if got := vt.Text(); got != "hello\nworld\naXc" {
		t.Fatalf("%q", got)
	}
}

func TestWrapAndScroll(t *testing.T) {
	vt := New(4, 2)
	write(vt, "abcdefgh")
	if got := vt.Text(); got != "abcd\nefgh" {
		t.Fatalf("wrap: %q", got)
	}
	// the cursor waits at the last column (deferred wrap) until more text
	if x, y := cursor(vt); x != 3 || y != 1 {
		t.Fatalf("cursor %d,%d", x, y)
	}
	write(vt, "ij")
	if got := vt.Text(); got != "efgh\nij" {
		t.Fatalf("scroll: %q", got)
	}
	if vt.Scrollback() != 1 {
		t.Fatalf("scrollback %d", vt.Scrollback())
	}
	var first string
	vt.View(1, func(s Screen) { first = string(runes(s.Rows[0])) })
	if first != "abcd" {
		t.Fatalf("scrolled view row 0: %q", first)
	}
}

func runes(r []Cell) []rune {
	var out []rune
	for _, c := range r {
		out = append(out, c.R)
	}
	return out
}

func TestUTF8(t *testing.T) {
	vt := New(10, 1)
	b := []byte("λ→✓é")
	for _, c := range b { // byte by byte: sequences split across writes
		vt.Write([]byte{c})
	}
	if got := vt.Text(); got != "λ→✓é" {
		t.Fatalf("%q", got)
	}
}

func TestCursorAndErase(t *testing.T) {
	vt := New(10, 4)
	write(vt, "0123456789\n0123456789\n0123456789")
	write(vt, "\x1b[2;3H")  // row 2, col 3
	write(vt, "\x1b[K")     // erase to end of line
	write(vt, "\x1b[1;1HX") // home
	write(vt, "\x1b[3;5H\x1b[1K")
	want := "X123456789\n01\n     56789"
	if got := vt.Text(); got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	write(vt, "\x1b[2J")
	if got := vt.Text(); got != "" {
		t.Fatalf("after 2J: %q", got)
	}
	write(vt, "\x1b[3;4Hab\x1b[2D\x1b[Aup")
	if x, y := cursor(vt); x != 5 || y != 1 {
		t.Fatalf("cursor %d,%d", x, y)
	}
}

func TestInsertDeleteChars(t *testing.T) {
	vt := New(8, 1)
	write(vt, "abcdef\x1b[1;3H\x1b[2P")
	if got := vt.Text(); got != "abef" {
		t.Fatalf("delete: %q", got)
	}
	write(vt, "\x1b[1;2H\x1b[2@")
	if got := vt.Text(); got != "a  bef" {
		t.Fatalf("insert: %q", got)
	}
	write(vt, "\x1b[1;1H\x1b[3X")
	if got := vt.Text(); got != "   bef" {
		t.Fatalf("erase chars: %q", got)
	}
}

func TestSGR(t *testing.T) {
	vt := New(10, 1)
	write(vt, "\x1b[1;31ma\x1b[0mb\x1b[38;5;208mc\x1b[48;2;1;2;3md\x1b[7;4me\x1b[m")
	if c := cell(vt, 0, 0); c.FG != 1 || c.Attr&Bold == 0 {
		t.Fatalf("a: %+v", c)
	}
	if c := cell(vt, 1, 0); c.FG != Default || c.Attr != 0 {
		t.Fatalf("b: %+v", c)
	}
	if c := cell(vt, 2, 0); c.FG != 208 {
		t.Fatalf("c: %+v", c)
	}
	if c := cell(vt, 3, 0); c.BG != RGB(1, 2, 3) {
		t.Fatalf("d: %+v", c)
	}
	if c := cell(vt, 4, 0); c.Attr&(Reverse|Underline) != Reverse|Underline {
		t.Fatalf("e: %+v", c)
	}
	write(vt, "\x1b[92;104mf")
	if c := cell(vt, 5, 0); c.FG != 10 || c.BG != 12 {
		t.Fatalf("bright: %+v", c)
	}
}

func TestScrollRegion(t *testing.T) {
	vt := New(5, 4)
	write(vt, "a\nb\nc\nd")
	write(vt, "\x1b[2;3r")      // region rows 2-3
	write(vt, "\x1b[3;1H\nnew") // LF at the region's bottom scrolls only it
	if got := vt.Text(); got != "a\nc\nnew\nd" {
		t.Fatalf("%q", got)
	}
	if vt.Scrollback() != 0 {
		t.Fatal("a region scroll went to the scrollback")
	}
	write(vt, "\x1b[r\x1b[2;1H\x1b[L") // insert a line at row 2
	if got := vt.Text(); got != "a\n\nc\nnew" {
		t.Fatalf("insert line: %q", got)
	}
	write(vt, "\x1b[M")
	if got := vt.Text(); got != "a\nc\nnew" {
		t.Fatalf("delete line: %q", got)
	}
}

func TestAltScreen(t *testing.T) {
	vt := New(6, 2)
	write(vt, "shell$")
	write(vt, "\x1b[?1049h\x1b[Hedit")
	if got := vt.Text(); got != "edit" {
		t.Fatalf("alt: %q", got)
	}
	write(vt, "\x1b[?1049l")
	if got := vt.Text(); got != "shell$" {
		t.Fatalf("back: %q", got)
	}
	if x, y := cursor(vt); x != 5 || y != 0 {
		t.Fatalf("cursor restored to %d,%d", x, y)
	}
}

func TestCursorVisibilityAndTitle(t *testing.T) {
	vt := New(5, 1)
	var on bool
	write(vt, "\x1b[?25l")
	vt.View(0, func(s Screen) { on = s.CursorOn })
	if on {
		t.Fatal("cursor still on")
	}
	write(vt, "\x1b]0;my title\x07x\x1b]2;other\x1b\\y")
	if vt.Title() != "other" || vt.Text() != "xy" {
		t.Fatalf("title %q text %q", vt.Title(), vt.Text())
	}
}

// godeep's line editor redraws with backspaces and spaces (kernel/tty).
func TestLineEditorRedraw(t *testing.T) {
	vt := New(20, 1)
	write(vt, "$ helo")
	write(vt, "\b\b")   // left twice
	write(vt, "l")      // insert: types l...
	write(vt, "lo\b\b") // ...redraws the tail and moves back
	if got := vt.Text(); got != "$ hello" {
		t.Fatalf("%q", got)
	}
}

func TestDirty(t *testing.T) {
	vt := New(5, 3)
	vt.View(0, func(Screen) {}) // clear the initial all-dirty
	write(vt, "\x1b[2;1Hx")
	var d []bool
	vt.View(0, func(s Screen) { d = append([]bool(nil), s.Dirty...) })
	if d[0] || !d[1] || d[2] {
		t.Fatalf("dirty %v, want only row 1", d)
	}
	select {
	case <-vt.Changed():
	default:
		t.Fatal("no change notification")
	}
}

// Garbage must never panic or leave the cursor off the grid.
func TestGarbage(t *testing.T) {
	vt := New(7, 3)
	seqs := []string{"\x1b[999;999H", "\x1b[0;0H", "\x1b[99999999999A", "\x1b[38;5m", "\x1b[38;2;1m", "\x1b[48;5;999m",
		"\x1b[?1049h", "\x1b[5;2r", "\x1b[0r", "\x1b[99L", "\x1b[99M", "\x1b[99P", "\x1b[99@", "\x1b[99X", "\x1b[99S", "\x1b[99T",
		"\x1b]" + strings.Repeat("x", 2000), "\x1b(B", "\x1b[>c", "\x1b[3J", "\x1bM\x1bM\x1bM", "\xff\xfe", "\x1b8", "\x1bc"}
	for i := 0; i < 500; i++ {
		write(vt, seqs[i%len(seqs)]+fmt.Sprint(i))
		x, y := cursor(vt)
		if x < 0 || x >= 7 || y < 0 || y >= 3 {
			t.Fatalf("after %q: cursor %d,%d", seqs[i%len(seqs)], x, y)
		}
	}
}

func FuzzWrite(f *testing.F) {
	f.Add([]byte("\x1b[2J\x1b[1;31mhi\x1b[0m\n\x1b[?1049h"))
	f.Fuzz(func(t *testing.T, b []byte) {
		vt := New(9, 4)
		vt.Write(b)
		x, y := cursor(vt)
		if x < 0 || x >= 9 || y < 0 || y >= 4 {
			t.Fatalf("cursor %d,%d", x, y)
		}
	})
}

func TestConcurrent(t *testing.T) {
	vt := New(40, 10)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				fmt.Fprintf(vt, "\x1b[3%dmline %d %d\n", i, i, j)
			}
		}()
	}
	for range 100 {
		vt.View(3, func(Screen) {})
	}
	wg.Wait()
}

func TestPalette(t *testing.T) {
	var ansi [16][3]uint8
	ansi[1] = [3]uint8{0xf7, 0x76, 0x8e}
	def := [3]uint8{9, 9, 9}
	if Palette(Default, &ansi, def) != def || Palette(1, &ansi, def) != ansi[1] {
		t.Fatal("default/ansi")
	}
	if Palette(RGB(1, 2, 3), &ansi, def) != [3]uint8{1, 2, 3} {
		t.Fatal("rgb")
	}
	if Palette(196, &ansi, def) != [3]uint8{255, 0, 0} || Palette(232, &ansi, def) != [3]uint8{8, 8, 8} {
		t.Fatal("cube/gray")
	}
}
