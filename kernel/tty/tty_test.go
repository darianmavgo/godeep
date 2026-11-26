package tty

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func feed(s string, closeAfter bool) <-chan byte {
	c := make(chan byte, len(s))
	for i := 0; i < len(s); i++ {
		c <- s[i]
	}
	if closeAfter {
		close(c)
	}
	return c
}

func TestReadLine(t *testing.T) {
	var out bytes.Buffer
	tt := New(feed("ab\x7fc\x1b[Ad\r\nnext\n\x03\x04", true), &out, true)
	ctx := context.Background()
	if l, err := tt.ReadLine(ctx); l != "acd" || err != nil {
		t.Fatalf("got %q %v", l, err)
	}
	if l, err := tt.ReadLine(ctx); l != "next" || err != nil {
		t.Fatalf("got %q %v", l, err)
	}
	if _, err := tt.ReadLine(ctx); !errors.Is(err, ErrInterrupt) {
		t.Fatalf("want interrupt, got %v", err)
	}
	if _, err := tt.ReadLine(ctx); err != io.EOF {
		t.Fatalf("want EOF on ^D, got %v", err)
	}
	if _, err := tt.ReadLine(ctx); err != io.EOF || !tt.Closed() {
		t.Fatalf("want EOF+closed, got %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("\b \b")) {
		t.Fatalf("no backspace echo: %q", out.String())
	}
}

// screen replays echoed output onto one line (\b moves left), the way a
// terminal shows it.
func screen(out string) string {
	var line []byte
	pos := 0
	for i := 0; i < len(out); i++ {
		switch c := out[i]; c {
		case '\b':
			if pos > 0 {
				pos--
			}
		case '\n':
			line, pos = nil, 0
		default:
			if pos < len(line) {
				line[pos] = c
			} else {
				line = append(line, c)
			}
			pos++
		}
	}
	return strings.TrimRight(string(line), " ")
}

func TestLineEditing(t *testing.T) {
	cases := []struct{ keys, want string }{
		{"helo\x1b[Dl\r", "hello"},           // left, insert
		{"world\x01hello \r", "hello world"}, // ^A, insert at the start
		{"abc\x1b[D\x1b[D\x1b[3~\r", "ac"},   // delete under the cursor
		{"abc\x1b[H\x1b[C\x7f\r", "bc"},      // home, right, backspace
		{"one two\x17three\r", "one three"},  // ^W
		{"junk\x15ok\r", "ok"},               // ^U
		{"ab\x1bOD\x1bOCc\r", "abc"},         // ESC O cursor keys
	}
	for _, c := range cases {
		var out bytes.Buffer
		tt := New(feed(c.keys, false), &out, true)
		l, err := tt.ReadLine(context.Background())
		tt.Close()
		if err != nil || l != c.want {
			t.Errorf("%q: got %q %v, want %q", c.keys, l, err, c.want)
		}
	}
	// What the terminal shows matches the line.
	var out bytes.Buffer
	tt := New(feed("wrld\x1b[D\x1b[D\x1b[Do\x1b[F!", false), &out, true)
	defer tt.Close()
	time.Sleep(50 * time.Millisecond)
	tt.mu.Lock() // the discipline writes under it
	shown := out.String()
	tt.mu.Unlock()
	if got := screen(shown); got != "world!" {
		t.Fatalf("screen shows %q", got)
	}
}

func TestHistory(t *testing.T) {
	var out bytes.Buffer
	keys := "first\rsecond\r\x1b[A\x1b[A\r" + // up twice: first
		"\x1b[A\x1b[A\x1b[B\r" + // history is first, second, first: up, up, down = first
		"typing\x1b[A\x1b[B more\r" // browsing keeps the line being typed
	tt := New(feed(keys, false), &out, true)
	defer tt.Close()
	ctx := context.Background()
	for _, want := range []string{"first", "second", "first", "first", "typing more"} {
		if l, err := tt.ReadLine(ctx); err != nil || l != want {
			t.Fatalf("got %q %v, want %q", l, err, want)
		}
	}
}
