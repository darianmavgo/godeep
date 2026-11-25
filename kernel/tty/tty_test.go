package tty

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
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
