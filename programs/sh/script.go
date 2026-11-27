package sh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"godeep/kernel/proc"
)

// State is what a shell remembers between command lines: variables, the
// last status, a script's arguments and the background jobs. Builtins
// that run as processes of their own (in a pipeline) share it, hence the
// lock.
type State struct {
	mu      sync.Mutex
	vars    map[string]string
	args    []string // $0, $1, ... (source)
	status  int
	jobs    []*job
	nextJob int
	depth   int // nested source
}

type job struct {
	id    int
	line  string
	procs []*proc.Proc
}

func (j *job) done() bool {
	for _, p := range j.procs {
		select {
		case <-p.Done():
		default:
			return false
		}
	}
	return true
}

// NewState returns an empty shell state.
func NewState() *State { return &State{vars: map[string]string{}} }

func (s *State) lookup(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case name == "?":
		return strconv.Itoa(s.status)
	case name == "#":
		return strconv.Itoa(max(len(s.args)-1, 0))
	case name[0] >= '0' && name[0] <= '9':
		if n := int(name[0] - '0'); n < len(s.args) {
			return s.args[n]
		}
		return ""
	}
	return s.vars[name]
}

func (s *State) set(name, val string) { s.mu.Lock(); s.vars[name] = val; s.mu.Unlock() }

func (s *State) setStatus(n int) { s.mu.Lock(); s.status = n; s.mu.Unlock() }

func (s *State) addJob(line string, procs []*proc.Proc) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextJob++
	s.jobs = append(s.jobs, &job{id: s.nextJob, line: line, procs: procs})
	return s.nextJob
}

// findJob returns job %N (or PID N's job); "" is the latest job.
func (s *State) findJob(spec string) (*job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if spec == "" {
		if len(s.jobs) == 0 {
			return nil, errors.New("no jobs")
		}
		return s.jobs[len(s.jobs)-1], nil
	}
	byID := strings.HasPrefix(spec, "%")
	n, err := strconv.Atoi(strings.TrimPrefix(spec, "%"))
	if err != nil {
		return nil, fmt.Errorf("%s: not a job (%%N) or PID", spec)
	}
	for _, j := range s.jobs {
		if byID && j.id == n {
			return j, nil
		}
		if !byID && slices.ContainsFunc(j.procs, func(p *proc.Proc) bool { return p.PID == n }) {
			return j, nil
		}
	}
	return nil, fmt.Errorf("%s: no such job", spec)
}

// prune drops finished jobs and returns them.
func (s *State) prune() []*job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var done []*job
	s.jobs = slices.DeleteFunc(s.jobs, func(j *job) bool {
		if j.done() {
			done = append(done, j)
			return true
		}
		return false
	})
	return done
}

// statusErr is a command's failure that needs no message (false, test).
type statusErr int

func (e statusErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// errInterrupted: the foreground pipeline was stopped with ^C; the rest
// of the command list doesn't run.
var errInterrupted = errors.New("interrupted")

func exitStatus(err error) int {
	var se statusErr
	switch {
	case err == nil:
		return 0
	case errors.As(err, &se):
		return int(se)
	case errors.Is(err, errInterrupted):
		return 130
	}
	return 1
}

// cmdError is a command's error and the command's name, for the message.
type cmdError struct {
	name string
	err  error
}

func (e *cmdError) Error() string { return e.err.Error() }
func (e *cmdError) Unwrap() error { return e.err }

// report prints a command's error, unless it needs none.
func report(w io.Writer, fallback string, err error) {
	var se statusErr
	if err == nil || errors.As(err, &se) || errors.Is(err, errInterrupted) {
		return
	}
	name := fallback
	var ce *cmdError
	if errors.As(err, &ce) {
		name = ce.name
	}
	fmt.Fprintf(w, "%s: %v\n", name, err)
}

// errOut is where the shell reports errors: the terminal, else stdout.
func (e Env) errOut() io.Writer {
	if e.TTY != nil {
		return e.TTY
	}
	return e.out()
}

// runLine runs one command line (a list of pipelines). The errors of
// commands it ran before the last one are reported as they happen; the
// last one's is returned (a *cmdError naming the command).
func runLine(ctx context.Context, env Env, p *proc.Proc, line string) error {
	if env.Sh == nil {
		env.Sh = NewState() // forgotten after this line
	}
	toks, err := lex(line)
	if err != nil {
		env.Sh.setStatus(2)
		return err
	}
	items, err := splitItems(toks)
	if err != nil {
		env.Sh.setStatus(2)
		return err
	}
	var pending error
	status := 0
	for i, it := range items {
		if i > 0 {
			switch items[i-1].sep {
			case "&&":
				if status != 0 {
					continue
				}
			case "||":
				if status == 0 {
					continue
				}
			}
		}
		report(env.errOut(), "sh", pending)
		pending = runItem(ctx, env, p, it)
		status = exitStatus(pending)
		env.Sh.setStatus(status)
		if errors.Is(pending, errInterrupted) || ctx.Err() != nil {
			break
		}
	}
	return pending
}

func runItem(ctx context.Context, env Env, p *proc.Proc, it item) error {
	pl, err := parsePipe(it.toks, env.Sh.lookup)
	if err != nil {
		return err
	}
	pl.bg = it.sep == "&"
	if len(pl.stages) == 0 {
		return nil
	}
	name := pl.stages[0][0]
	// NAME=VALUE alone: shell variables
	if st := pl.stages[0]; len(pl.stages) == 1 && !pl.bg && pl.in == "" && pl.out == "" && !slices.ContainsFunc(st, func(w string) bool { return !isEnvAssign(w) }) {
		for _, w := range st {
			k, v, _ := strings.Cut(w, "=")
			env.Sh.set(k, v)
		}
		return nil
	}
	// time PIPELINE: how long the whole pipeline took (to the terminal).
	var start time.Time
	if name == "time" && !pl.bg {
		if len(pl.stages[0]) == 1 {
			return errors.New("usage: time COMMAND [| COMMAND...]")
		}
		pl.stages[0] = pl.stages[0][1:]
		name = pl.stages[0][0]
		start = time.Now()
		defer func() { fmt.Fprintf(env.errOut(), "real %s\n", time.Since(start).Round(time.Millisecond)) }()
	}
	if err := execute(ctx, env, p, pl); err != nil {
		return &cmdError{name: name, err: err}
	}
	return nil
}

func init() {
	for name, b := range map[string]builtin{
		"set":    {"set: shell variables (NAME=VALUE sets one, $NAME uses it)", setCmd},
		"unset":  {"unset NAME...: forget shell variables", unset},
		"source": {"source VOLUME:PATH [ARGS...]: run a script in this shell ($1.. are ARGS; also: . FILE)", source},
		".":      {"same as source", source},
		"jobs":   {"background jobs (started with &)", jobs},
		"wait":   {"wait [%N|PID...]: wait for background jobs (all by default); ^C stops waiting", waitCmd},
		"fg":     {"fg [%N|PID]: wait for a job in the foreground; ^C kills it (it gets no terminal input)", fg},
		"true":   {"succeed", func(context.Context, Env, *proc.Proc, []string) error { return nil }},
		"false":  {"fail (status 1)", func(context.Context, Env, *proc.Proc, []string) error { return statusErr(1) }},
		"test":   {"test EXPR: -e/-f/-d VOLUME:PATH, -n/-z S, S = S, S != S, N -eq/-ne/-lt/-le/-gt/-ge N, ! EXPR", testCmd},
		"[":      {"[ EXPR ]: test", bracket},
	} {
		builtins[name] = b
	}
}

func setCmd(_ context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: set (NAME=VALUE sets a variable)")
	}
	env.Sh.mu.Lock()
	vars := maps.Clone(env.Sh.vars)
	env.Sh.mu.Unlock()
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		fmt.Fprintf(env.out(), "%s=%s\n", k, vars[k])
	}
	return nil
}

func unset(_ context.Context, env Env, _ *proc.Proc, args []string) error {
	env.Sh.mu.Lock()
	defer env.Sh.mu.Unlock()
	for _, k := range args {
		delete(env.Sh.vars, k)
	}
	return nil
}

const maxSourceDepth = 16

// source runs a script line by line in this shell: its variables are the
// shell's; $0 is the file, $1... the arguments. It stops at ^C (or when
// killed) and returns the last line's status.
func source(ctx context.Context, env Env, p *proc.Proc, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: source VOLUME:PATH [ARGS...]")
	}
	f, err := openIn(ctx, env, args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(ctxReader{ctx, f})
	if err != nil {
		return err
	}
	sh := env.Sh
	sh.mu.Lock()
	if sh.depth >= maxSourceDepth {
		sh.mu.Unlock()
		return errors.New("scripts nested too deep")
	}
	sh.depth++
	saved := sh.args
	sh.args = slices.Clone(args)
	sh.mu.Unlock()
	defer func() {
		sh.mu.Lock()
		sh.depth--
		sh.args = saved
		sh.mu.Unlock()
	}()

	var last error
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		report(env.errOut(), "sh", last)
		last = runLine(ctx, env, p, line)
		if last != nil && !errors.Is(last, errInterrupted) {
			if _, ok := last.(*cmdError); !ok && exitStatus(last) == 1 {
				last = &cmdError{name: fmt.Sprintf("%s:%d", args[0], n), err: last}
			}
		}
		if errors.Is(last, errInterrupted) || ctx.Err() != nil {
			break
		}
	}
	return last
}

func jobs(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	env.Sh.mu.Lock()
	js := slices.Clone(env.Sh.jobs)
	env.Sh.mu.Unlock()
	for _, j := range js {
		state := "running"
		if j.done() {
			state = "done"
		}
		pids := make([]string, len(j.procs))
		for i, p := range j.procs {
			pids[i] = strconv.Itoa(p.PID)
		}
		fmt.Fprintf(env.out(), "%%%d\t%s\t%s\t%s\n", j.id, state, strings.Join(pids, " "), j.line)
	}
	env.Sh.prune()
	return nil
}

// waitJobs waits for every process of the jobs. On ^C it kills them if
// kill is set, else it stops waiting. It returns the status of the last
// job's last process.
func waitJobs(ctx context.Context, env Env, js []*job, kill bool) error {
	var intr <-chan struct{}
	if env.TTY != nil {
		intr = env.TTY.Interrupts()
	}
	var last error
	for _, j := range js {
		for _, p := range j.procs {
		wait:
			for {
				select {
				case <-p.Done():
					break wait
				case <-intr:
					if !kill {
						return errInterrupted
					}
					for _, q := range j.procs {
						q.Kill()
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		last = quiet(j.procs[len(j.procs)-1].Err())
	}
	env.Sh.prune()
	return last
}

func waitCmd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	var js []*job
	if len(args) == 0 {
		env.Sh.mu.Lock()
		js = slices.Clone(env.Sh.jobs)
		env.Sh.mu.Unlock()
	}
	for _, a := range args {
		j, err := env.Sh.findJob(a)
		if err != nil {
			return err
		}
		js = append(js, j)
	}
	return waitJobs(ctx, env, js, false)
}

func fg(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: fg [%N|PID]")
	}
	spec := ""
	if len(args) == 1 {
		spec = args[0]
	}
	j, err := env.Sh.findJob(spec)
	if err != nil {
		return err
	}
	fmt.Fprintln(env.out(), j.line)
	return waitJobs(ctx, env, []*job{j}, true)
}

func bracket(ctx context.Context, env Env, p *proc.Proc, args []string) error {
	if len(args) == 0 || args[len(args)-1] != "]" {
		return errors.New("[: missing ]")
	}
	return testCmd(ctx, env, p, args[:len(args)-1])
}

// testCmd: status 0 if EXPR is true, 1 if false, 2 (with a message) if
// it's malformed.
func testCmd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	ok, err := evalTest(ctx, env, args)
	if err != nil {
		return err
	}
	if !ok {
		return statusErr(1)
	}
	return nil
}

func evalTest(ctx context.Context, env Env, a []string) (bool, error) {
	if len(a) > 0 && a[0] == "!" {
		ok, err := evalTest(ctx, env, a[1:])
		return !ok, err
	}
	switch len(a) {
	case 0:
		return false, nil
	case 1:
		return a[0] != "", nil
	case 2:
		switch a[0] {
		case "-n":
			return a[1] != "", nil
		case "-z":
			return a[1] == "", nil
		case "-e", "-f", "-d":
			fsys, rel, err := openPath(ctx, env, a[1])
			if err != nil {
				return false, nil
			}
			st, err := fs.Stat(fsys, rel)
			if err != nil {
				return false, nil
			}
			return a[0] == "-e" || (a[0] == "-d") == st.IsDir(), nil
		}
	case 3:
		switch a[1] {
		case "=", "==":
			return a[0] == a[2], nil
		case "!=":
			return a[0] != a[2], nil
		case "-eq", "-ne", "-lt", "-le", "-gt", "-ge":
			x, err1 := strconv.ParseInt(a[0], 10, 64)
			y, err2 := strconv.ParseInt(a[2], 10, 64)
			if err1 != nil || err2 != nil {
				return false, fmt.Errorf("%s %s %s: integers expected", a[0], a[1], a[2])
			}
			return map[string]bool{"-eq": x == y, "-ne": x != y, "-lt": x < y, "-le": x <= y, "-gt": x > y, "-ge": x >= y}[a[1]], nil
		}
	}
	return false, fmt.Errorf("test: bad expression %q", strings.Join(a, " "))
}
