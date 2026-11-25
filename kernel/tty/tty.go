// Package tty is the console line discipline. A kernel goroutine ("tty")
// owns the raw input stream: it echoes, edits and assembles lines, and turns
// ^C into an interrupt, independently of who (if anyone) is reading. This is
// what lets you type ahead while a foreground program runs.
package tty

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"godeep/kernel/proc"
)

const (
	CtrlC = 0x03
	CtrlD = 0x04
	esc   = 0x1b
)

// ErrInterrupt is returned by ReadLine when ^C is typed at the prompt.
var ErrInterrupt = errors.New("interrupt")

type event struct {
	line string
	err  error
}

// TTY is a console. Writes are serialized; input is cooked into lines.
type TTY struct {
	echo   bool
	lines  chan event
	intr   chan struct{}
	closed atomic.Bool

	mu  sync.Mutex
	out io.Writer
}

// New starts the line discipline over a driver's byte channel. If echo is
// set, typed characters are echoed (raw serial); a cooked host terminal
// echoes by itself.
func New(in <-chan byte, out io.Writer, echo bool) *TTY {
	t := &TTY{echo: echo, out: out, lines: make(chan event, 16), intr: make(chan struct{})}
	proc.Kthread("tty", func() { t.discipline(in) })
	return t
}

func (t *TTY) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.out.Write(p)
}

// Interrupts delivers ^C to a foreground waiter. When nobody is receiving,
// ^C goes to the line reader as ErrInterrupt instead.
func (t *TTY) Interrupts() <-chan struct{} { return t.intr }

// Closed reports whether the input device has gone away (host stdin EOF).
func (t *TTY) Closed() bool { return t.closed.Load() }

// ReadLine returns the next line, ErrInterrupt on ^C, io.EOF on ^D at the
// start of a line or once the device is closed, or ctx.Err().
func (t *TTY) ReadLine(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case ev, ok := <-t.lines:
		if !ok {
			t.closed.Store(true)
			return "", io.EOF
		}
		return ev.line, ev.err
	}
}

func (t *TTY) echoStr(s string) {
	if t.echo {
		io.WriteString(t, s)
	}
}

func (t *TTY) discipline(in <-chan byte) {
	defer close(t.lines)
	var line []byte
	skip := 0
	lastCR := false
	for b := range in {
		if skip > 0 {
			skip--
			if skip == 1 && b != '[' {
				skip = 0
			}
			continue
		}
		wasCR := lastCR
		lastCR = b == '\r'
		switch {
		case b == '\n' && wasCR:
			// CRLF: the CR already ended the line
		case b == '\r' || b == '\n':
			t.echoStr("\n")
			t.lines <- event{line: string(line)}
			line = nil
		case b == CtrlC:
			t.echoStr("^C\n")
			line = nil
			select {
			case t.intr <- struct{}{}: // a foreground job's waiter took it
			default:
				t.lines <- event{err: ErrInterrupt}
			}
		case b == CtrlD:
			if len(line) == 0 {
				t.lines <- event{err: io.EOF}
			}
		case b == 0x7f || b == 0x08:
			if len(line) > 0 {
				line = line[:len(line)-1]
				t.echoStr("\b \b")
			}
		case b == esc:
			skip = 2 // ignore ESC [ X (arrow keys)
		case b >= 0x20 && b < 0x7f:
			line = append(line, b)
			t.echoStr(string(b))
		}
	}
}
