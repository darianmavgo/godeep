package edit

import (
	"strings"
	"testing"
)

// typ feeds terminal bytes through a Decoder into the editor.
func typ(e *Editor, s string) Action {
	var d Decoder
	a := Continue
	for i := 0; i < len(s); i++ {
		for _, k := range d.Feed(s[i]) {
			a = e.Key(k)
		}
	}
	for _, k := range d.Flush() {
		a = e.Key(k)
	}
	return a
}

func TestEditing(t *testing.T) {
	e := New("f.txt", []byte("hello\nworld\n"), 20, 5)
	typ(e, "\x1b[F, there")        // End, type
	typ(e, "\x1b[B\x1b[H>> ")       // Down, Home, type
	typ(e, "\r")                    // split the line at the cursor
	typ(e, "\x7f\x7f\x7f\x7f")      // join back and delete ">> "
	if got := string(e.Text()); got != "hello, there\nworld\n" {
		t.Fatalf("text %q", got)
	}
	if !e.Dirty() {
		t.Fatal("not dirty")
	}
	typ(e, "\x1b[A\x1b[H\x1b[3~") // Up, Home, Delete
	if got := string(e.Text()); got != "ello, there\nworld\n" {
		t.Fatalf("delete %q", got)
	}
	// ^K cuts consecutive lines together; ^U pastes them
	e = New("f", []byte("a\nb\nc\nd\n"), 20, 5)
	typ(e, "\x0b\x0b\x1b[B\x15")
	if got := string(e.Text()); got != "c\na\nb\nd\n" {
		t.Fatalf("cut/paste %q", got)
	}
	// unicode and tabs
	e = New("f", nil, 20, 5)
	typ(e, "\tñ€x")
	rows, r, c := e.Screen()
	if rows[0] != "    ñ€x"+strings.Repeat(" ", 13) || r != 0 || c != 7 {
		t.Fatalf("tab/unicode row %q cursor %d,%d", rows[0], r, c)
	}
	if string(e.Text()) != "\tñ€x\n" {
		t.Fatalf("text %q", e.Text())
	}
}

func TestQuitAndSave(t *testing.T) {
	e := New("f", []byte("x\n"), 20, 5)
	if typ(e, "\x11") != Quit {
		t.Fatal("clean ^Q didn't quit")
	}
	typ(e, "y")
	if typ(e, "\x11") != Continue {
		t.Fatal("dirty ^Q quit at once")
	}
	if rows, _, _ := e.Screen(); !strings.HasPrefix(rows[4], "unsaved") {
		t.Fatalf("status %q", rows[4])
	}
	if typ(e, "\x11") != Quit {
		t.Fatal("second ^Q didn't quit")
	}
	e = New("f", []byte("x\n"), 20, 5)
	typ(e, "y")
	if typ(e, "\x13") != Save {
		t.Fatal("^S")
	}
	e.Saved(3)
	if e.Dirty() || typ(e, "\x11") != Quit {
		t.Fatal("saved buffer still dirty")
	}
}

func TestScrollFindGoto(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 100; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("\n")
	}
	b.WriteString("needle here\n")
	e := New("f", []byte(b.String()), 20, 6)
	typ(e, "\x06needle\r")
	if r, c := e.Cursor(); r != 100 || c != 0 {
		t.Fatalf("find: %d,%d", r, c)
	}
	rows, cr, _ := e.Screen()
	if !strings.HasPrefix(rows[cr], "needle here") {
		t.Fatalf("not scrolled to the match: %q", rows)
	}
	typ(e, "\x06\r") // again: wraps to itself
	if r, _ := e.Cursor(); r != 100 {
		t.Fatalf("find again: %d", r)
	}
	typ(e, "\x07" + "42\r")
	if r, _ := e.Cursor(); r != 41 {
		t.Fatalf("goto: %d", r)
	}
	typ(e, "\x07abc\r")
	if rows, _, _ := e.Screen(); !strings.HasPrefix(rows[5], "not a line") {
		t.Fatalf("bad goto status %q", rows[5])
	}
	typ(e, "\x06zzz\x1b") // ESC cancels the prompt
	if rows, _, _ := e.Screen(); !strings.HasPrefix(rows[5], "cancelled") {
		t.Fatalf("cancel status %q", rows[5])
	}
	typ(e, "\x1b[6~\x1b[6~\x1b[5~")
	if r, _ := e.Cursor(); r != 41+4 {
		t.Fatalf("page down/up: %d", r)
	}
	// a long line scrolls horizontally
	e = New("f", []byte(strings.Repeat("0123456789", 5)+"\n"), 10, 3)
	typ(e, "\x1b[F")
	rows, _, c := e.Screen()
	if c != 9 || rows[0] != "123456789 " {
		t.Fatalf("horizontal: %q cursor col %d", rows[0], c)
	}
}

func FuzzEditor(f *testing.F) {
	f.Add([]byte("abc\x1b[A\r\x7f\x0b\x15\x06a\r\x07" + "3\r\x1b[3~\x1b[6~"), []byte("x\ny\n"))
	f.Fuzz(func(t *testing.T, keys, text []byte) {
		e := New("f", text, 12, 4)
		typ(e, string(keys))
		rows, r, c := e.Screen()
		if len(rows) != 4 || r < 0 || r >= 4 || c < 0 || c >= 12 {
			t.Fatalf("screen %d rows, cursor %d,%d", len(rows), r, c)
		}
		for _, row := range rows {
			if n := len([]rune(row)); n != 12 {
				t.Fatalf("row width %d: %q", n, row)
			}
		}
	})
}
