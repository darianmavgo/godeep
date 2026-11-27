package simplefb

import (
	"strings"
	"testing"
)

func TestANSI(t *testing.T) {
	c, fb := newTestConsole(t) // 40x10
	c.Write([]byte("line one\nline two\nline three\n"))
	c.Write([]byte("\x1b[2J\x1b[H"))
	for i, l := range screenText(c) {
		if l != "" {
			t.Fatalf("row %d not cleared: %q", i, l)
		}
	}
	c.Write([]byte("\x1b[3;5Hhello\x1b[1;1Htop\x1b[10;38Hend"))
	got := screenText(c)
	if got[0] != "top" || got[2] != "    hello" || got[9] != strings.Repeat(" ", 37)+"end" {
		t.Fatalf("positioning:\n%q", got)
	}
	c.Write([]byte("\x1b[3;7H\x1b[K"))
	if got := screenText(c)[2]; got != "    he" {
		t.Fatalf("erase to end of line: %q", got)
	}
	// reverse video is kept in the text and drawn swapped; selection text drops it
	c.Write([]byte("\x1b[5;1H\x1b[7mSTATUS\x1b[0m plain\x1b[99;99H"))
	c.mu.Lock()
	r := c.lines[c.top()+4][0]
	c.mu.Unlock()
	if r&revBit == 0 || r&^revBit != 'S' {
		t.Fatalf("reverse cell %x", r)
	}
	c.mu.Lock()
	row5 := make([]rune, 0, c.cols)
	for _, r := range c.lines[c.top()+4] {
		row5 = append(row5, r&^revBit)
	}
	c.mu.Unlock()
	if got := string(row5); !strings.HasPrefix(got, "STATUS plain") {
		t.Fatalf("row 5: %q", got)
	}
	if c.row != 9 || c.col != 39 {
		t.Fatalf("clamped cursor at %d,%d", c.row, c.col)
	}
	// unknown sequences vanish; split writes still parse
	c.Write([]byte("\x1b[H\x1b[38;5;10mX\x1b["))
	c.Write([]byte("2;2HY\x1b[?1049h"))
	if got := screenText(c); got[0] != "Xop" || got[1] != " Y" {
		t.Fatalf("split/unknown:\n%q", got[:2])
	}
	// the cursor shows (inverted cell) only when enabled, and the picture
	// matches the text once it's hidden again
	c.Write([]byte("\x1b[?25h\x1b[1;1H"))
	c.mu.Lock()
	shown := c.curShown
	c.mu.Unlock()
	if !shown {
		t.Fatal("cursor not shown")
	}
	c.Write([]byte("\x1b[?25l"))
	pixelsMatch(t, c, fb)
}
