package sh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"

	"godeep/kernel/ipc"
	"godeep/kernel/proc"
	"godeep/kernel/prog"
	"godeep/kernel/tty"
	"godeep/kernel/vfs"
)

// Command lines: words separated by blanks, 'single' and "double" quotes
// (\" \\ \$ inside double quotes), \X outside quotes, # comments, $NAME,
// ${NAME}, $? (the last status), $# and $0..$9 (a script's arguments),
// expanded when the command runs (not split into words), and
//
//	a | b | c        pipeline: each stage is its own process, joined by pipes
//	< VOL:PATH       the first stage reads the file
//	> VOL:PATH       the last stage writes the file (>> appends)
//	a ; b            a, then b
//	a && b, a || b   b if a succeeded, failed
//	a &              a in the background (a job: jobs, wait, fg)
//	NAME=VALUE       alone: sets a shell variable; before a command: its
//	                 environment (WASI programs)
//
// A builtin runs inside the shell when it's alone in the foreground (so
// `exit`, `kill` and `restrict` act on this shell), and as a process of
// its own in a pipeline or in the background.

// part is a piece of a word: literal text, or a $reference.
type part struct {
	text string
	ref  string // NAME, ?, #, or a digit; "" for text
}

type token struct {
	s     string // an operator, or a word without $references
	op    bool   // an unquoted | < > >> & ; && ||
	parts []part // a word with $references (s unused)
}

// word is the token's text with its $references looked up (nil: empty).
func (t token) word(lookup func(string) string) string {
	if t.parts == nil {
		return t.s
	}
	var b strings.Builder
	for _, p := range t.parts {
		if p.ref == "" {
			b.WriteString(p.text)
		} else if lookup != nil {
			b.WriteString(lookup(p.ref))
		}
	}
	return b.String()
}

func identStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func identChar(c byte) bool  { return identStart(c) || c >= '0' && c <= '9' }

// varRef reads the reference at line[i] == '$': its name and the index
// after it; ok false: a plain $.
func varRef(line string, i int) (name string, next int, ok bool, err error) {
	if i+1 >= len(line) {
		return "", 0, false, nil
	}
	c := line[i+1]
	switch {
	case c == '?' || c == '#' || c >= '0' && c <= '9':
		return string(c), i + 2, true, nil
	case c == '{':
		j := strings.IndexByte(line[i+2:], '}')
		if j < 0 {
			return "", 0, false, errors.New("unterminated ${")
		}
		name = line[i+2 : i+2+j]
		if !validRef(name) {
			return "", 0, false, fmt.Errorf("${%s}: bad substitution", name)
		}
		return name, i + 3 + j, true, nil
	case identStart(c):
		j := i + 1
		for j < len(line) && identChar(line[j]) {
			j++
		}
		return line[i+1 : j], j, true, nil
	}
	return "", 0, false, nil
}

func validRef(name string) bool {
	if name == "?" || name == "#" || len(name) == 1 && name[0] >= '0' && name[0] <= '9' {
		return true
	}
	if name == "" || !identStart(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !identChar(name[i]) {
			return false
		}
	}
	return true
}

func lex(line string) ([]token, error) {
	var out []token
	var cur strings.Builder
	var parts []part
	have := false // cur holds a word (maybe empty: "")
	flush := func() {
		if !have {
			return
		}
		if parts != nil {
			if cur.Len() > 0 {
				parts = append(parts, part{text: cur.String()})
			}
			out = append(out, token{parts: parts})
		} else {
			out = append(out, token{s: cur.String()})
		}
		cur.Reset()
		parts = nil
		have = false
	}
	// dollar handles a $ at line[i]; it returns the index of the last byte used.
	dollar := func(i int) (int, error) {
		name, next, ok, err := varRef(line, i)
		if err != nil {
			return 0, err
		}
		have = true
		if !ok {
			cur.WriteByte('$')
			return i, nil
		}
		if cur.Len() > 0 {
			parts = append(parts, part{text: cur.String()})
			cur.Reset()
		}
		parts = append(parts, part{ref: name})
		return next - 1, nil
	}
	op := func(s string) {
		flush()
		out = append(out, token{s: s, op: true})
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			flush()
		case c == '#' && !have:
			flush()
			return out, nil
		case c == '\\':
			have = true
			if i+1 < len(line) {
				i++
			}
			cur.WriteByte(line[i])
		case c == '$':
			j, err := dollar(i)
			if err != nil {
				return nil, err
			}
			i = j
		case c == '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated '")
			}
			cur.WriteString(line[i+1 : i+1+j])
			have = true
			i += j + 1
		case c == '"':
			have = true
			i++
			for ; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < len(line) && strings.IndexByte(`"\$`, line[i+1]) >= 0 {
					i++
				} else if line[i] == '$' {
					j, err := dollar(i)
					if err != nil {
						return nil, err
					}
					i = j
					continue
				}
				cur.WriteByte(line[i])
			}
			if i >= len(line) {
				return nil, errors.New(`unterminated "`)
			}
		case c == '|' || c == '&' || c == '>':
			if i+1 < len(line) && line[i+1] == c {
				op(line[i : i+2])
				i++
			} else {
				op(string(c))
			}
		case c == '<' || c == ';':
			op(string(c))
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	flush()
	return out, nil
}

// item is one pipeline of a command list and the operator after it.
type item struct {
	toks []token
	sep  string // "", ";", "&", "&&" or "||"
}

func splitItems(toks []token) ([]item, error) {
	var items []item
	var cur []token
	for _, t := range toks {
		if t.op && (t.s == ";" || t.s == "&" || t.s == "&&" || t.s == "||") {
			if len(cur) == 0 {
				return nil, fmt.Errorf("syntax error near %s", t.s)
			}
			items = append(items, item{cur, t.s})
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 {
		items = append(items, item{cur, ""})
	} else if n := len(items); n > 0 && (items[n-1].sep == "&&" || items[n-1].sep == "||") {
		return nil, fmt.Errorf("syntax error: %s at the end", items[n-1].sep)
	}
	return items, nil
}

type pipeline struct {
	stages [][]string
	in     string // < file
	out    string // > or >> file
	append bool
	bg     bool
}

// parse parses a line holding at most one pipeline (tests; no variables).
func parse(line string) (*pipeline, error) {
	toks, err := lex(line)
	if err != nil {
		return nil, err
	}
	items, err := splitItems(toks)
	if err != nil {
		return nil, err
	}
	switch len(items) {
	case 0:
		return &pipeline{}, nil
	case 1:
	default:
		return nil, errors.New("more than one command")
	}
	pl, err := parsePipe(items[0].toks, nil)
	if err != nil {
		return nil, err
	}
	pl.bg = items[0].sep == "&"
	return pl, nil
}

// parsePipe builds a pipeline from its tokens, expanding $references.
func parsePipe(toks []token, lookup func(string) string) (*pipeline, error) {
	pl := &pipeline{}
	var cur []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if !t.op {
			cur = append(cur, t.word(lookup))
			continue
		}
		switch t.s {
		case "|":
			if len(cur) == 0 {
				return nil, errors.New("syntax error near |")
			}
			if pl.out != "" {
				return nil, errors.New("> must be in the last command")
			}
			pl.stages = append(pl.stages, cur)
			cur = nil
		case "<", ">", ">>":
			if i+1 >= len(toks) || toks[i+1].op {
				return nil, fmt.Errorf("syntax error: %s needs a file", t.s)
			}
			i++
			f := toks[i].word(lookup)
			if t.s == "<" {
				if len(pl.stages) > 0 || pl.in != "" {
					return nil, errors.New("< must be in the first command")
				}
				pl.in = f
			} else {
				if pl.out != "" {
					return nil, errors.New("more than one >")
				}
				pl.out, pl.append = f, t.s == ">>"
			}
		default:
			return nil, fmt.Errorf("syntax error near %s", t.s)
		}
	}
	if len(cur) == 0 {
		if len(pl.stages) > 0 {
			return nil, errors.New("syntax error: | at the end")
		}
		if pl.in != "" || pl.out != "" {
			return nil, errors.New("syntax error: redirection without a command")
		}
		return pl, nil
	}
	pl.stages = append(pl.stages, cur)
	return pl, nil
}

// stage is one command of a pipeline, ready to spawn.
type stage struct {
	name string
	args []string
	main proc.Main
	// inline: a builtin run in the shell itself (alone, in the foreground)
	inline func(ctx context.Context) error
}

func resolve(env Env, args []string) (stage, error) {
	var envVars []string
	for len(args) > 1 && isEnvAssign(args[0]) {
		envVars = append(envVars, args[0])
		args = args[1:]
	}
	if args[0] == "exit" {
		return stage{name: "exit", main: func(context.Context, *proc.Proc) error { return nil }}, nil
	}
	if b, ok := builtins[args[0]]; ok {
		rest := args[1:]
		return stage{name: args[0], args: rest, main: func(ctx context.Context, cp *proc.Proc) error {
			e := env
			e.Out, e.In = cp.Stdout, cp.Stdin
			return b.run(ctx, e, cp, rest)
		}}, nil
	}
	if pg, ok := prog.Lookup(args[0]); ok {
		return stage{name: args[0], args: args[1:], main: pg.Main}, nil
	}
	if strings.HasSuffix(args[0], ".wasm") && strings.Contains(args[0], ":") {
		return stage{name: path.Base(args[0]), args: args[1:], main: wasmMain(env, args[0], envVars)}, nil
	}
	return stage{}, fmt.Errorf("%s: command not found", args[0])
}

func execute(ctx context.Context, env Env, p *proc.Proc, pl *pipeline) error {
	// Alone in the foreground, a builtin runs in the shell.
	if len(pl.stages) == 1 && !pl.bg && pl.in == "" && pl.out == "" {
		args := pl.stages[0]
		if b, ok := builtins[args[0]]; ok {
			return b.run(ctx, env, p, args[1:])
		}
	}
	stages := make([]stage, len(pl.stages))
	for i, args := range pl.stages {
		s, err := resolve(env, args)
		if err != nil {
			return err
		}
		stages[i] = s
	}

	n := len(stages)
	specs := make([]proc.Spec, n)
	var files []io.Closer // closed once every stage is done
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for i, s := range stages {
		specs[i] = proc.Spec{Name: s.name, Args: s.args, Main: s.main}
	}

	// Input of the first stage: a file, the terminal (foreground), or none.
	switch {
	case pl.in != "":
		f, err := openIn(ctx, env, pl.in)
		if err != nil {
			return err
		}
		c := onceCloser(f)
		files = append(files, c)
		specs[0].Stdin, specs[0].Ends = f, append(specs[0].Ends, c)
	case env.In != nil:
		specs[0].Stdin = env.In // the shell's own stdin (a shell in a pipeline)
	case !pl.bg && env.TTY != nil:
		r := newTTYReader(ctx, env.TTY)
		files = append(files, r)
		specs[0].Stdin, specs[0].Ends = r, append(specs[0].Ends, r)
	}

	// Output of the last stage: a file or the shell's output.
	specs[n-1].Stdout = env.out()
	if pl.out != "" {
		w, err := openOut(ctx, env, pl.out, pl.append)
		if err != nil {
			return err
		}
		c := onceCloser(w)
		files = append(files, c)
		specs[n-1].Stdout, specs[n-1].Ends = w, append(specs[n-1].Ends, c)
	}

	// Pipes between stages: each end is closed when its process ends.
	for i := 0; i < n-1; i++ {
		r, w := ipc.Pipe()
		specs[i].Stdout, specs[i].Ends = w, append(specs[i].Ends, w)
		specs[i+1].Stdin, specs[i+1].Ends = r, append(specs[i+1].Ends, r)
	}

	children := make([]*proc.Proc, n)
	for i := range specs {
		children[i] = env.Table.Spawn(ctx, p.PID, specs[i])
	}
	if pl.bg {
		files = nil // the processes close them (Ends)
		pids := make([]string, n)
		for i, c := range children {
			pids[i] = fmt.Sprint(c.PID)
		}
		id := env.Sh.addJob(pl.String(), children)
		fmt.Fprintf(env.out(), "[%s] %s (%%%d)\n", strings.Join(pids, " "), stages[n-1].name, id)
		return nil
	}

	// Foreground: wait for every stage; ^C kills them all.
	killAll := func() {
		for _, c := range children {
			c.Kill()
		}
	}
	var intr <-chan struct{}
	if env.TTY != nil {
		intr = env.TTY.Interrupts()
	}
	interrupted := false
	for _, c := range children {
	wait:
		for {
			select {
			case <-c.Done():
				break wait
			case <-intr:
				interrupted = true
				killAll()
			case <-ctx.Done():
				killAll()
				return ctx.Err()
			}
		}
	}
	// Earlier stages: report real failures (not "the reader went away").
	for _, c := range children[:n-1] {
		if err := c.Err(); quiet(err) == nil {
			continue
		} else if env.TTY != nil {
			fmt.Fprintf(env.TTY, "%s: %v\n", c.Name, err)
		}
	}
	if interrupted {
		return errInterrupted
	}
	return quiet(children[n-1].Err())
}

// String is the pipeline as a command line (for jobs).
func (pl *pipeline) String() string {
	var b strings.Builder
	for i, st := range pl.stages {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(strings.Join(st, " "))
		if i == 0 && pl.in != "" {
			b.WriteString(" < " + pl.in)
		}
	}
	if pl.out != "" {
		if pl.append {
			b.WriteString(" >> " + pl.out)
		} else {
			b.WriteString(" > " + pl.out)
		}
	}
	return b.String()
}

// quiet drops the errors that are a normal way for a stage to end.
func quiet(err error) error {
	if err == nil || errors.Is(err, proc.ErrKilled) || ipc.IsClosedPipe(err) {
		return nil
	}
	return err
}

func openIn(ctx context.Context, env Env, arg string) (io.ReadCloser, error) {
	f, p, err := openPath(ctx, env, arg)
	if err != nil {
		return nil, err
	}
	return f.Open(p)
}

func openOut(ctx context.Context, env Env, arg string, appendTo bool) (io.WriteCloser, error) {
	w, p, err := writable(ctx, env, arg)
	if err != nil {
		return nil, err
	}
	if !appendTo {
		return w.Create(p)
	}
	st, err := fs.Stat(w, p)
	if errors.Is(err, fs.ErrNotExist) {
		if err := w.WriteFile(p, nil); err != nil {
			return nil, err
		}
		return &appender{w: w, path: p}, nil
	}
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s: is a directory", arg)
	}
	return &appender{w: w, path: p, off: st.Size()}, nil
}

// appender writes at the end of an existing file (>>).
type appender struct {
	mu   sync.Mutex
	w    vfs.Writable
	path string
	off  int64
}

func (a *appender) Write(b []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, err := a.w.WriteAt(a.path, b, a.off)
	a.off += int64(n)
	return n, err
}

func (a *appender) Close() error { return nil }

// onceCloser closes c once; a second Close waits for the first to finish
// (sync.Once), so the shell knows a redirected file is complete when its
// own Close returns, even if the dying process's Ends closed it first.
func onceCloser(c io.Closer) io.Closer { return &once{c: c} }

type once struct {
	o   sync.Once
	c   io.Closer
	err error
}

func (o *once) Close() error {
	o.o.Do(func() { o.err = o.c.Close() })
	return o.err
}

// ttyReader is a process's stdin from the terminal: a line at a time, EOF
// on ^D (or ^C typed while nothing waits for it). Close wakes a blocked
// Read: it's in the process's Ends.
type ttyReader struct {
	t      *tty.TTY
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	buf    []byte
}

func newTTYReader(ctx context.Context, t *tty.TTY) *ttyReader {
	ctx, cancel := context.WithCancel(ctx)
	return &ttyReader{t: t, ctx: ctx, cancel: cancel}
}

func (r *ttyReader) Read(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		line, err := r.t.ReadLine(r.ctx)
		if errors.Is(err, tty.ErrInterrupt) {
			return 0, io.EOF
		}
		if err != nil {
			if r.ctx.Err() != nil {
				return 0, io.EOF
			}
			return 0, err
		}
		r.buf = []byte(line + "\n")
	}
	n := copy(b, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *ttyReader) Close() error { r.cancel(); return nil }

// input is a builtin's stdin: the pipe or file it was given, else the
// terminal (the returned func releases it).
func (e Env) input(ctx context.Context) (io.Reader, func()) {
	if e.In != nil {
		return e.In, func() {}
	}
	if e.TTY == nil {
		return eofReader{}, func() {}
	}
	r := newTTYReader(ctx, e.TTY)
	return r, func() { r.Close() }
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }
