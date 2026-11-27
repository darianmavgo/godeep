// Package tty is the console line discipline. A kernel goroutine ("tty")
// owns the raw input stream: it echoes, edits and assembles lines, and turns
// ^C into an interrupt, independently of who (if anyone) is reading. This is
// what lets you type ahead while a foreground program runs.
package tty

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"godeep/kernel/proc"
)

const (
	CtrlC = 0x03
	CtrlD = 0x04
	esc0  = 0x1b
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
	stop   chan struct{}
	once   sync.Once
	closed atomic.Bool
	raw    atomic.Pointer[rawReader] // set: input bypasses the line editor
	size   atomic.Uint64             // cols<<32 | rows; 0: unknown

	mu  sync.Mutex
	out io.Writer
}

type rawReader struct {
	c    chan byte
	done chan struct{}
}

// ErrBusy is returned by Raw when another program already reads raw input.
var ErrBusy = errors.New("tty: raw input already taken")

// Raw hands every input byte to the returned channel, unechoed and
// unedited (^C included), until release is called; then line editing
// resumes. For full-screen programs (an editor). One at a time.
func (t *TTY) Raw() (in <-chan byte, release func(), err error) {
	r := &rawReader{c: make(chan byte, 64), done: make(chan struct{})}
	if !t.raw.CompareAndSwap(nil, r) {
		return nil, nil, ErrBusy
	}
	var once sync.Once
	return r.c, func() {
		once.Do(func() {
			t.raw.CompareAndSwap(r, nil)
			close(r.done)
		})
	}, nil
}

// SetSize records the terminal's size in characters (a framebuffer's, an
// SSH client's window).
func (t *TTY) SetSize(cols, rows int) {
	if cols > 0 && rows > 0 {
		t.size.Store(uint64(cols)<<32 | uint64(uint32(rows)))
	}
}

// Size is the terminal's size in characters; 0, 0 if unknown (a serial
// line: ask the terminal, or assume 80x24).
func (t *TTY) Size() (cols, rows int) {
	v := t.size.Load()
	return int(v >> 32), int(uint32(v))
}

// New starts the line discipline over a driver's byte channel. If echo is
// set, typed characters are echoed (raw serial); a cooked host terminal
// echoes by itself.
func New(in <-chan byte, out io.Writer, echo bool) *TTY {
	t := newTTY(out, echo)
	proc.Kthread("tty", func() { t.discipline(in) })
	return t
}

// NewFor is New with the line discipline running as one of p's goroutines
// instead of a kernel thread: it is accounted to p and stops when p dies
// (or on Close). For terminals that come and go, like SSH sessions.
func NewFor(p *proc.Proc, in <-chan byte, out io.Writer, echo bool) *TTY {
	t := newTTY(out, echo)
	if !p.Go(func(ctx context.Context) {
		stop := context.AfterFunc(ctx, t.Close)
		defer stop()
		t.discipline(in)
	}) {
		t.Close()
		close(t.lines)
	}
	return t
}

func newTTY(out io.Writer, echo bool) *TTY {
	return &TTY{echo: echo, out: out, lines: make(chan event, 16), intr: make(chan struct{}), stop: make(chan struct{})}
}

// Close stops the line discipline goroutine. Pending ReadLine calls get io.EOF.
func (t *TTY) Close() { t.once.Do(func() { close(t.stop) }) }

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

// HistorySize is how many lines the up/down keys can recall.
const HistorySize = 100

// editor is the line being typed: a cursor that moves (left/right, home,
// end), insertion and deletion anywhere, and a history recalled with
// up/down. Redrawing uses only backspaces, spaces and characters, so it
// works the same on a serial terminal, over SSH and on the framebuffer.
type editor struct {
	t     *TTY
	line  []byte
	pos   int
	hist  [][]byte
	hi    int    // history entry shown; len(hist) means the line being typed
	saved []byte // the line being typed while browsing history
}

func (e *editor) out(s string) { e.t.echoStr(s) }

func (e *editor) back(n int) { e.out(strings.Repeat("\b", n)) }

// redrawTail prints the line from the cursor on, plus extra blanks to cover
// what was there, and moves the cursor back.
func (e *editor) redrawTail(extra int) {
	e.out(string(e.line[e.pos:]) + strings.Repeat(" ", extra))
	e.back(len(e.line) - e.pos + extra)
}

func (e *editor) insert(b byte) {
	e.line = append(e.line[:e.pos], append([]byte{b}, e.line[e.pos:]...)...)
	e.out(string(b))
	e.pos++
	e.redrawTail(0)
}

func (e *editor) backspace() {
	if e.pos == 0 {
		return
	}
	e.line = append(e.line[:e.pos-1], e.line[e.pos:]...)
	e.pos--
	e.back(1)
	e.redrawTail(1)
}

func (e *editor) del() {
	if e.pos == len(e.line) {
		return
	}
	e.line = append(e.line[:e.pos], e.line[e.pos+1:]...)
	e.redrawTail(1)
}

func (e *editor) left() {
	if e.pos > 0 {
		e.pos--
		e.back(1)
	}
}

func (e *editor) right() {
	if e.pos < len(e.line) {
		e.out(string(e.line[e.pos]))
		e.pos++
	}
}

func (e *editor) home() { e.back(e.pos); e.pos = 0 }

func (e *editor) end() { e.out(string(e.line[e.pos:])); e.pos = len(e.line) }

// replace shows another line (from history, or an empty one).
func (e *editor) replace(l []byte) {
	e.end()
	e.out(strings.Repeat("\b \b", len(e.line)))
	e.line = append([]byte(nil), l...)
	e.pos = len(e.line)
	e.out(string(e.line))
}

func (e *editor) killWord() {
	for e.pos > 0 && e.line[e.pos-1] == ' ' {
		e.backspace()
	}
	for e.pos > 0 && e.line[e.pos-1] != ' ' {
		e.backspace()
	}
}

func (e *editor) up() {
	if e.hi == 0 {
		return
	}
	if e.hi == len(e.hist) {
		e.saved = append([]byte(nil), e.line...)
	}
	e.hi--
	e.replace(e.hist[e.hi])
}

func (e *editor) down() {
	if e.hi == len(e.hist) {
		return
	}
	e.hi++
	if e.hi == len(e.hist) {
		e.replace(e.saved)
	} else {
		e.replace(e.hist[e.hi])
	}
}

// take ends the line: it goes to history (if new) and is returned.
func (e *editor) take() string {
	l := string(e.line)
	if l != "" && (len(e.hist) == 0 || string(e.hist[len(e.hist)-1]) != l) {
		e.hist = append(e.hist, []byte(l))
		if len(e.hist) > HistorySize {
			e.hist = e.hist[1:]
		}
	}
	e.line, e.pos, e.hi, e.saved = nil, 0, len(e.hist), nil
	return l
}

// escape handles a finished escape sequence: ESC [ (or ESC O) params final.
func (e *editor) escape(params string, final byte) {
	switch final {
	case 'A':
		e.up()
	case 'B':
		e.down()
	case 'C':
		e.right()
	case 'D':
		e.left()
	case 'H':
		e.home()
	case 'F':
		e.end()
	case '~':
		switch params {
		case "1", "7":
			e.home()
		case "4", "8":
			e.end()
		case "3":
			e.del()
		}
	}
}

func (t *TTY) discipline(in <-chan byte) {
	defer close(t.lines)
	e := &editor{t: t}
	esc := 0 // 1: after ESC; 2: inside ESC [ ... (params so far in params)
	params := ""
	lastCR := false
	for {
		var b byte
		select {
		case <-t.stop:
			return
		case c, ok := <-in:
			if !ok {
				return
			}
			b = c
		}
		if r := t.raw.Load(); r != nil {
			esc, lastCR = 0, false
			select {
			case r.c <- b:
			case <-r.done:
			case <-t.stop:
				return
			}
			continue
		}
		switch esc {
		case 1:
			if b == '[' || b == 'O' {
				esc, params = 2, ""
			} else {
				esc = 0
			}
			continue
		case 2:
			if b >= 0x40 && b <= 0x7e {
				esc = 0
				e.escape(params, b)
			} else {
				params += string(b)
			}
			continue
		}
		wasCR := lastCR
		lastCR = b == '\r'
		switch {
		case b == '\n' && wasCR:
			// CRLF: the CR already ended the line
		case b == '\r' || b == '\n':
			e.end()
			t.echoStr("\n")
			if !t.deliver(event{line: e.take()}) {
				return
			}
		case b == CtrlC:
			e.end()
			t.echoStr("^C\n")
			e.line, e.pos, e.hi, e.saved = nil, 0, len(e.hist), nil
			select {
			case t.intr <- struct{}{}: // a foreground job's waiter took it
			default:
				if !t.deliver(event{err: ErrInterrupt}) {
					return
				}
			}
		case b == CtrlD:
			if len(e.line) == 0 && !t.deliver(event{err: io.EOF}) {
				return
			}
		case b == 0x7f || b == 0x08:
			e.backspace()
		case b == 0x01: // ^A
			e.home()
		case b == 0x05: // ^E
			e.end()
		case b == 0x15: // ^U
			e.end()
			for len(e.line) > 0 {
				e.backspace()
			}
		case b == 0x17: // ^W
			e.killWord()
		case b == esc0:
			esc = 1
		case b >= 0x20 && b < 0x7f:
			e.insert(b)
		}
	}
}

// deliver queues an event for ReadLine; false if the TTY was closed.
func (t *TTY) deliver(ev event) bool {
	select {
	case t.lines <- ev:
		return true
	case <-t.stop:
		return false
	}
}
