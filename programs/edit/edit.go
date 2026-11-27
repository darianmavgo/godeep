// Package edit is a small full-screen text editor (nano-like keys), kept
// free of I/O: the caller feeds it keys (Decoder turns terminal bytes into
// Keys) and draws Screen() however its terminal wants. The shell's `edit`
// builtin does that over the TTY's raw mode with ANSI sequences, which the
// serial line, SSH and the framebuffer console all understand.
//
//	^S save   ^Q quit (twice if unsaved)   ^K cut line   ^U paste
//	^F find   ^G go to line   ^A/^E line start/end   arrows, Home, End,
//	PgUp/PgDn, Backspace, Delete, Enter, Tab
package edit

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// TabWidth is how many columns a tab advances to.
const TabWidth = 4

// Key is one keypress.
type Key struct {
	Rune rune // a printable character, or 0
	Code Code // a special key, or 0
}

// Code is a non-printing key.
type Code int

const (
	None Code = iota
	Up
	Down
	Left
	Right
	Home
	End
	PageUp
	PageDown
	Backspace
	Delete
	Enter
	Tab
	Escape
	CtrlA
	CtrlC
	CtrlE
	CtrlF
	CtrlG
	CtrlK
	CtrlQ
	CtrlS
	CtrlU
)

// Action is what the caller must do after a key.
type Action int

const (
	Continue Action = iota
	Save            // write Text() to the file, then call Saved or Failed
	Quit
)

// Editor is the buffer, the cursor and the view.
type Editor struct {
	lines    [][]rune
	row, col int // cursor: line, rune index
	want     int // visual column kept across up/down
	top      int // first line shown
	left     int // first visual column shown
	w, h     int // screen; the last row is the status line
	name     string
	dirty    bool
	msg      string
	clip     [][]rune
	cutRun   bool // the last key was ^K: the next one adds to the clip
	quitWarn bool
	prompt   *prompt
	lastFind string
}

type prompt struct {
	label string
	text  []rune
	done  func(e *Editor, answer string)
}

// New starts an editor on text (lines split on \n; a final newline is
// implied) for a w x h screen.
func New(name string, text []byte, w, h int) *Editor {
	e := &Editor{name: name}
	s := strings.TrimSuffix(strings.ReplaceAll(string(text), "\r\n", "\n"), "\n")
	for _, l := range strings.Split(s, "\n") {
		e.lines = append(e.lines, []rune(l))
	}
	e.Resize(w, h)
	return e
}

// Resize sets the screen size (at least 10x3).
func (e *Editor) Resize(w, h int) {
	e.w, e.h = max(w, 10), max(h, 3)
	e.scroll()
}

// Text is the buffer with a newline after every line.
func (e *Editor) Text() []byte {
	var b strings.Builder
	for _, l := range e.lines {
		b.WriteString(string(l))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// Dirty reports unsaved changes.
func (e *Editor) Dirty() bool { return e.dirty }

// Saved tells the editor its Save succeeded (n bytes).
func (e *Editor) Saved(n int) {
	e.dirty = false
	e.msg = fmt.Sprintf("saved %d lines, %d bytes", len(e.lines), n)
}

// Failed shows a save error.
func (e *Editor) Failed(err error) { e.msg = "not saved: " + err.Error() }

// Message shows a one-off message in the status line.
func (e *Editor) Message(s string) { e.msg = s }

// Cursor is the cursor's line and rune index (0-based).
func (e *Editor) Cursor() (row, col int) { return e.row, e.col }

// Key handles one keypress.
func (e *Editor) Key(k Key) Action {
	if e.prompt != nil {
		e.promptKey(k)
		return Continue
	}
	quitWarn := e.quitWarn
	e.quitWarn = false
	cut := false
	if k.Code != None || k.Rune != 0 {
		e.msg = ""
	}
	switch k.Code {
	case None:
		if k.Rune != 0 {
			e.insert(k.Rune)
		}
	case Up:
		e.vertical(-1)
	case Down:
		e.vertical(1)
	case PageUp:
		e.vertical(-(e.h - 2))
	case PageDown:
		e.vertical(e.h - 2)
	case Left:
		if e.col > 0 {
			e.col--
		} else if e.row > 0 {
			e.row--
			e.col = len(e.lines[e.row])
		}
		e.want = e.vcol()
	case Right:
		if e.col < len(e.lines[e.row]) {
			e.col++
		} else if e.row < len(e.lines)-1 {
			e.row, e.col = e.row+1, 0
		}
		e.want = e.vcol()
	case Home, CtrlA:
		e.col, e.want = 0, 0
	case End, CtrlE:
		e.col = len(e.lines[e.row])
		e.want = e.vcol()
	case Enter:
		l := e.lines[e.row]
		rest := append([]rune(nil), l[e.col:]...)
		e.lines[e.row] = l[:e.col]
		e.lines = append(e.lines[:e.row+1], append([][]rune{rest}, e.lines[e.row+1:]...)...)
		e.row, e.col, e.want = e.row+1, 0, 0
		e.dirty = true
	case Tab:
		e.insert('\t')
	case Backspace:
		if e.col > 0 {
			l := e.lines[e.row]
			e.lines[e.row] = append(l[:e.col-1], l[e.col:]...)
			e.col--
		} else if e.row > 0 {
			prev := e.lines[e.row-1]
			e.col = len(prev)
			e.lines[e.row-1] = append(prev, e.lines[e.row]...)
			e.lines = append(e.lines[:e.row], e.lines[e.row+1:]...)
			e.row--
		} else {
			break
		}
		e.want = e.vcol()
		e.dirty = true
	case Delete:
		l := e.lines[e.row]
		if e.col < len(l) {
			e.lines[e.row] = append(l[:e.col], l[e.col+1:]...)
		} else if e.row < len(e.lines)-1 {
			e.lines[e.row] = append(l, e.lines[e.row+1]...)
			e.lines = append(e.lines[:e.row+1], e.lines[e.row+2:]...)
		} else {
			break
		}
		e.dirty = true
	case CtrlK:
		line := append([]rune(nil), e.lines[e.row]...)
		if e.cutRun {
			e.clip = append(e.clip, line)
		} else {
			e.clip = [][]rune{line}
		}
		cut = true
		if len(e.lines) == 1 {
			e.lines[0] = nil
		} else {
			e.lines = append(e.lines[:e.row], e.lines[e.row+1:]...)
			e.row = min(e.row, len(e.lines)-1)
		}
		e.col = min(e.col, len(e.lines[e.row]))
		e.dirty = true
	case CtrlU:
		if len(e.clip) > 0 {
			paste := make([][]rune, len(e.clip))
			for i, l := range e.clip {
				paste[i] = append([]rune(nil), l...)
			}
			e.lines = append(e.lines[:e.row], append(paste, e.lines[e.row:]...)...)
			e.row += len(paste)
			e.col = 0
			e.dirty = true
		}
	case CtrlF:
		e.ask("find: ", e.lastFind, func(e *Editor, s string) {
			if s != "" {
				e.lastFind = s
				e.find(s)
			}
		})
	case CtrlG:
		e.ask("go to line: ", "", func(e *Editor, s string) {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || n < 1 {
				e.msg = "not a line number"
				return
			}
			e.row, e.col, e.want = min(n, len(e.lines))-1, 0, 0
		})
	case CtrlS:
		e.scroll()
		return Save
	case CtrlQ:
		if e.dirty && !quitWarn {
			e.quitWarn = true
			e.msg = "unsaved changes: ^Q again to discard them, ^S to save"
			break
		}
		return Quit
	case CtrlC:
		e.msg = "^Q quits, ^S saves"
	}
	e.cutRun = cut
	e.scroll()
	return Continue
}

func (e *Editor) insert(r rune) {
	l := e.lines[e.row]
	l = append(l[:e.col], append([]rune{r}, l[e.col:]...)...)
	e.lines[e.row] = l
	e.col++
	e.want = e.vcol()
	e.dirty = true
}

func (e *Editor) vertical(n int) {
	e.row = min(max(e.row+n, 0), len(e.lines)-1)
	e.col = colAt(e.lines[e.row], e.want)
}

// find moves to the next match after the cursor, wrapping around.
func (e *Editor) find(s string) {
	needle := []rune(s)
	n := len(e.lines)
	for i := 0; i <= n; i++ {
		r := (e.row + i) % n
		start := 0
		if i == 0 {
			start = e.col + 1
		}
		if start > len(e.lines[r]) {
			continue
		}
		if j := index(e.lines[r][start:], needle); j >= 0 {
			if i == n || r < e.row {
				e.msg = "search wrapped"
			}
			e.row, e.col = r, start+j
			e.want = e.vcol()
			return
		}
	}
	e.msg = fmt.Sprintf("%q not found", s)
}

// index is the rune index of needle in hay, or -1.
func index(hay, needle []rune) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j, r := range needle {
			if hay[i+j] != r {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func (e *Editor) ask(label, initial string, done func(*Editor, string)) {
	e.prompt = &prompt{label: label, text: []rune(initial), done: done}
}

func (e *Editor) promptKey(k Key) {
	p := e.prompt
	switch {
	case k.Code == Enter:
		e.prompt = nil
		p.done(e, string(p.text))
		e.scroll()
	case k.Code == Escape || k.Code == CtrlC || k.Code == CtrlQ:
		e.prompt = nil
		e.msg = "cancelled"
	case k.Code == Backspace:
		if len(p.text) > 0 {
			p.text = p.text[:len(p.text)-1]
		}
	case k.Code == None && k.Rune != 0:
		p.text = append(p.text, k.Rune)
	}
}

// vcol is the cursor's visual column (tabs expanded).
func (e *Editor) vcol() int { return visual(e.lines[e.row], e.col) }

func visual(l []rune, col int) int {
	v := 0
	for _, r := range l[:col] {
		if r == '\t' {
			v += TabWidth - v%TabWidth
		} else {
			v++
		}
	}
	return v
}

// colAt is the rune index at visual column v (or the line's end).
func colAt(l []rune, v int) int {
	x := 0
	for i, r := range l {
		next := x + 1
		if r == '\t' {
			next = x + TabWidth - x%TabWidth
		}
		if next > v {
			return i
		}
		x = next
	}
	return len(l)
}

// scroll keeps the cursor inside the view.
func (e *Editor) scroll() {
	text := e.h - 1
	if e.row < e.top {
		e.top = e.row
	}
	if e.row >= e.top+text {
		e.top = e.row - text + 1
	}
	v := e.vcol()
	if v < e.left {
		e.left = v
	}
	if v >= e.left+e.w {
		e.left = v - e.w + 1
	}
}

// Screen returns the screen's rows, each exactly w columns (the last is
// the status line, meant to be drawn in reverse video), and the cursor's
// screen position (0-based).
func (e *Editor) Screen() (rows []string, curRow, curCol int) {
	for i := 0; i < e.h-1; i++ {
		var row string
		if n := e.top + i; n < len(e.lines) {
			row = expand(e.lines[n], e.left, e.w)
		} else {
			row = "~"
		}
		rows = append(rows, pad(row, e.w))
	}
	curRow, curCol = e.row-e.top, e.vcol()-e.left
	var status string
	switch {
	case e.prompt != nil:
		status = e.prompt.label + string(e.prompt.text)
		curRow, curCol = e.h-1, min(utf8.RuneCountInString(status), e.w-1)
	case e.msg != "":
		status = e.msg
	default:
		mod := ""
		if e.dirty {
			mod = " [modified]"
		}
		status = fmt.Sprintf("%s%s  %d/%d:%d  ^S save ^Q quit ^K cut ^U paste ^F find ^G line",
			e.name, mod, e.row+1, len(e.lines), e.vcol()+1)
	}
	rows = append(rows, pad(status, e.w))
	return rows, curRow, curCol
}

// expand renders a line from visual column left, w columns wide.
func expand(l []rune, left, w int) string {
	var out []rune
	x := 0
	for _, r := range l {
		n := 1
		if r == '\t' {
			n = TabWidth - x%TabWidth
			r = ' '
		} else if r < 0x20 || r == 0x7f {
			r = '?'
		}
		for i := 0; i < n; i++ {
			if x >= left && x < left+w {
				out = append(out, r)
			}
			x++
		}
		if x >= left+w {
			break
		}
	}
	return string(out)
}

func pad(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		return string(r[:w])
	}
	return s + strings.Repeat(" ", w-len(r))
}
