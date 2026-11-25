package proc

import (
	"runtime"
	"strconv"
	"strings"
)

// Goroutine is one goroutine as reported by the Go runtime.
type Goroutine struct {
	ID        int
	State     string            // e.g. "running", "chan receive", "select", "sleep"
	Waiting   string            // e.g. "2 minutes" (only for long waits)
	Locked    bool              // locked to an OS thread / M
	Labels    map[string]string // pprof labels; pid/proc for godeep goroutines
	PID       int               // from the pid label, -1 if unlabeled (runtime)
	Func      string            // innermost function
	Where     string            // innermost non-runtime/stdlib function
	CreatedBy string
}

// Snapshot returns every goroutine in the system. It parses the runtime's own
// traceback (runtime.Stack with all=true), which in Go 1.27+ includes labels.
func Snapshot() []Goroutine {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return parseStacks(string(buf[:n]))
		}
		buf = make([]byte, 2*len(buf))
	}
}

func parseStacks(s string) []Goroutine {
	var out []Goroutine
	for _, block := range strings.Split(strings.TrimSpace(s), "\n\n") {
		if g, ok := parseBlock(block); ok {
			out = append(out, g)
		}
	}
	return out
}

func parseBlock(block string) (Goroutine, bool) {
	lines := strings.Split(block, "\n")
	g := Goroutine{PID: -1}
	h := lines[0]
	if !strings.HasPrefix(h, "goroutine ") {
		return g, false
	}
	open, close := strings.IndexByte(h, '['), strings.IndexByte(h, ']')
	if open < 0 || close < open {
		return g, false
	}
	idField := strings.Fields(h[len("goroutine "):open])
	if len(idField) == 0 {
		return g, false
	}
	g.ID, _ = strconv.Atoi(idField[0])

	parts := strings.Split(h[open+1:close], ", ")
	g.State = parts[0]
	var extra []string
	for _, x := range parts[1:] {
		if x == "locked to thread" {
			g.Locked = true
		} else {
			extra = append(extra, x)
		}
	}
	g.Waiting = strings.Join(extra, ", ")

	rest := h[close+1:]
	if lo, hi := strings.IndexByte(rest, '{'), strings.LastIndexByte(rest, '}'); lo >= 0 && hi > lo {
		g.Labels = map[string]string{}
		for _, kv := range strings.Split(rest[lo+1:hi], ", ") {
			k, v, ok := strings.Cut(kv, ": ")
			if ok {
				g.Labels[k] = strings.Trim(v, `"`)
			}
		}
		if pid, err := strconv.Atoi(g.Labels["pid"]); err == nil {
			g.PID = pid
		}
	}

	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "\t") || strings.HasPrefix(l, "...") {
			continue // file:line or elided frames
		}
		if strings.HasPrefix(l, "created by ") {
			g.CreatedBy = strings.TrimPrefix(l, "created by ")
			continue
		}
		fn := l
		if i := strings.LastIndexByte(fn, '('); i > 0 {
			fn = fn[:i]
		}
		if g.Func == "" {
			g.Func = fn
		}
		if g.Where == "" && !isRuntime(fn) {
			g.Where = fn
		}
	}
	if g.Where == "" {
		g.Where = g.Func
	}
	return g, true
}

func isRuntime(fn string) bool {
	for _, p := range []string{"runtime.", "runtime/", "internal/", "sync.", "time.", "os.", "context.", "bufio.", "syscall."} {
		if strings.HasPrefix(fn, p) {
			return true
		}
	}
	return false
}
