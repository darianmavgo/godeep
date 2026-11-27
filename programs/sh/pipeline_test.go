package sh

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	"godeep/fs/memfs"
	"godeep/fs/procfs"
	"godeep/kernel/ipc"
	"godeep/kernel/proc"
	"godeep/kernel/tty"
	"godeep/kernel/vfs"
	_ "godeep/programs/coreutils"
)

func TestLexParse(t *testing.T) {
	for _, c := range []struct {
		line string
		want pipeline
	}{
		{"echo hi", pipeline{stages: [][]string{{"echo", "hi"}}}},
		{"ps|grep sh | wc -l", pipeline{stages: [][]string{{"ps"}, {"grep", "sh"}, {"wc", "-l"}}}},
		{"echo 'a | b' \"c \\\"d\\\"\" > ram:/x", pipeline{stages: [][]string{{"echo", "a | b", `c "d"`}}, out: "ram:/x"}},
		{"cat < ram:/in | sort >> ram:/out", pipeline{stages: [][]string{{"cat"}, {"sort"}}, in: "ram:/in", out: "ram:/out", append: true}},
		{"sleep 5&", pipeline{stages: [][]string{{"sleep", "5"}}, bg: true}},
		{"echo ''", pipeline{stages: [][]string{{"echo", ""}}}},
		{"   ", pipeline{}},
	} {
		got, err := parse(c.line)
		if err != nil {
			t.Errorf("%q: %v", c.line, err)
			continue
		}
		if !reflect.DeepEqual(*got, c.want) {
			t.Errorf("%q: got %+v, want %+v", c.line, *got, c.want)
		}
	}
	for _, bad := range []string{"| a", "a |", "a | | b", "a > ", "a & b", "a > x | b", "a | b < x", "'open", `"open`, "> x", "a > x > y"} {
		if _, err := parse(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// shell runs command lines like sh -c and returns what reached the terminal.
type shell struct {
	t   *testing.T
	ctx context.Context
	env Env
	out *syncBuf
	sh  *proc.Proc
	in  chan byte // typed into the terminal
}

func newShell(t *testing.T) *shell {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := &syncBuf{}
	in := make(chan byte, 16)
	term := tty.New(in, out, false)
	t.Cleanup(term.Close)
	tab := proc.NewTable()
	ns := vfs.New(ctx, nil)
	ns.Mount("ram", memfs.New("ram"))
	ns.Mount("proc", procfs.Proc(tab, time.Now(), "test"))
	env := Env{Table: tab, TTY: term, FS: ns, Boot: time.Now(), Ports: ipc.NewNamespace(), Sh: NewState()}
	// The shell's own process, so stages have a parent.
	self := tab.Spawn(ctx, 0, proc.Spec{Name: "sh", Main: func(ctx context.Context, _ *proc.Proc) error { <-ctx.Done(); return nil }})
	return &shell{t: t, ctx: ctx, env: env, out: out, sh: self, in: in}
}

func (s *shell) run(line string) (string, error) {
	s.t.Helper()
	before := len(s.out.String())
	err := runLine(s.ctx, s.env, s.sh, line)
	return s.out.String()[before:], err
}

func (s *shell) must(line string) string {
	s.t.Helper()
	out, err := s.run(line)
	if err != nil {
		s.t.Fatalf("%s: %v (output %q)", line, err, out)
	}
	return out
}

func TestPipelines(t *testing.T) {
	s := newShell(t)
	if got := s.must("echo hello world | wc"); got != "1 2 12\n" {
		t.Fatalf("wc: %q", got)
	}
	if got := s.must("yes | head -3"); got != "y\ny\ny\n" {
		t.Fatalf("yes | head: %q", got)
	}
	if got := s.must("yes abc | head -n 1000 | tail -2 | wc -l"); got != "2\n" {
		t.Fatalf("long pipeline: %q", got)
	}
	// A builtin in a pipeline is a process of its own.
	if got := s.must("help | grep -c '^ps '"); got != "1\n" {
		t.Fatalf("help | grep: %q", got)
	}
	s.must("echo b > ram:/f")
	s.must("echo a >> ram:/f")
	s.must("echo c >> ram:/f")
	if got := s.must("cat < ram:/f | sort"); got != "a\nb\nc\n" {
		t.Fatalf("sort: %q", got)
	}
	if got := s.must("sort -r < ram:/f > ram:/g"); got != "" {
		t.Fatalf("redirected output reached the terminal: %q", got)
	}
	if got := s.must("cat ram:/g"); got != "c\nb\na\n" {
		t.Fatalf("sorted file: %q", got)
	}
	if got := s.must("cat proc:/1/cmdline | head -1"); got != "sh\n" {
		t.Fatalf("proc: %q", got)
	}
	if _, err := s.run("nosuch | wc"); err == nil {
		t.Fatal("unknown command in a pipeline")
	}
	if _, err := s.run("echo x > proc:/1/status"); !errors.Is(err, vfs.ErrReadOnly) {
		t.Fatalf("write to proc: %v", err)
	}
	// Every stage is gone once the line returns.
	waitFor(t, func() bool { return len(s.env.Table.List()) == 1 })
}

func TestPipelineKill(t *testing.T) {
	s := newShell(t)
	// The background pipeline runs until killed; killing the reader ends
	// the writer too (its pipe closes).
	out := s.must("yes | grep -v n > ram:/y &")
	if !strings.HasPrefix(out, "[") {
		t.Fatalf("background: %q", out)
	}
	waitFor(t, func() bool {
		st, err := fs.Stat(mustOpen(t, s, "ram"), "y")
		return err == nil && st.Size() > 0
	})
	for _, p := range s.env.Table.List() {
		if p.Name == "grep" {
			p.Kill()
		}
	}
	waitFor(t, func() bool { return len(s.env.Table.List()) == 1 })
	b, err := fs.ReadFile(mustOpen(t, s, "ram"), "y")
	if err != nil || !strings.HasPrefix(string(b), "y\ny\n") {
		t.Fatalf("ram:/y = %q, %v", b[:min(len(b), 20)], err)
	}
}

func mustOpen(t *testing.T, s *shell, name string) vfs.Volume {
	v, err := s.env.FS.Open(s.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestRestrict(t *testing.T) {
	s := newShell(t)
	s.must("echo hi > ram:/a")
	if got := s.must("restrict -vol ram -ro cat ram:/a"); got != "hi\n" {
		t.Fatalf("restricted read: %q", got)
	}
	if _, err := s.run("restrict -vol ram -ro write ram:/b x"); err == nil {
		t.Fatal("read-only view wrote")
	}
	if _, err := s.run("restrict -vol ram cat proc:/1/cmdline"); err == nil {
		t.Fatal("view opened proc")
	}
	if got := s.must("restrict -vol ram df"); strings.Contains(got, "proc") {
		t.Fatalf("df shows hidden volumes:\n%s", got)
	}
	// Narrowing is one-way.
	if _, err := s.run("restrict -vol ram restrict -vol ram,proc cat proc:/1/cmdline"); err == nil {
		t.Fatal("nested restrict widened")
	}
	if _, err := s.run("restrict -nodev halt"); err == nil {
		t.Fatal("restricted halt")
	}
	if _, err := s.run("restrict -net none get http://x/"); err == nil {
		t.Fatal("restricted get")
	}
	// In a pipeline too.
	if got := s.must("restrict -vol ram cat ram:/a | wc -l"); got != "1\n" {
		t.Fatalf("restrict in a pipeline: %q", got)
	}
}

func TestKV(t *testing.T) {
	s := newShell(t)
	s.must("kvd &")
	waitFor(t, func() bool { return len(s.env.Ports.List()) == 1 })
	s.must("kv set color blue")
	if got := s.must("kv get color"); got != "blue\n" {
		t.Fatalf("get: %q", got)
	}
	s.must("kv set shape round")
	if got := s.must("kv list | wc -l"); got != "2\n" {
		t.Fatalf("list: %q", got)
	}
	if got := s.must("ports"); !strings.Contains(got, "kv") || !strings.Contains(got, "sh.KVReq → string") {
		t.Fatalf("ports:\n%s", got)
	}
	if _, err := s.run("restrict -ports other kv get color"); err == nil {
		t.Fatal("restricted kv reached the port")
	}
	if _, err := s.run("kvd"); err == nil {
		t.Fatal("second kvd on the same name")
	}
	for _, p := range s.env.Table.List() {
		if p.Name == "kvd" {
			p.Kill()
		}
	}
	waitFor(t, func() bool { return len(s.env.Ports.List()) == 0 })
}

func TestTop(t *testing.T) {
	s := newShell(t)
	s.must("yes | grep -c n &") // runs until killed, in bounded memory
	got := s.must("top -n 1 -d 0.3")
	if !strings.Contains(got, "%RUN") || !strings.Contains(got, "yes") {
		t.Fatalf("top:\n%s", got)
	}
	for _, p := range s.env.Table.List() {
		if p.Name == "grep" {
			p.Kill() // and yes then fails on the closed pipe
		}
	}
	waitFor(t, func() bool { return len(s.env.Table.List()) == 1 })
}

func TestRestrictLimits(t *testing.T) {
	s := newShell(t)
	start := time.Now()
	_, err := s.run("restrict -time 200ms sleep 10")
	if !errors.Is(err, proc.ErrLimit) || time.Since(start) > 5*time.Second {
		t.Fatalf("time limit: %v after %v", err, time.Since(start))
	}
	out, err := s.run("restrict -out 1K yes")
	if !errors.Is(err, proc.ErrLimit) || len(out) != 1024 {
		t.Fatalf("output limit: %v, %d bytes", err, len(out))
	}
	// A limited builtin runs as a process, so the limit applies to it too.
	if _, err := s.run("restrict -out 5 help"); !errors.Is(err, proc.ErrLimit) {
		t.Fatalf("limited builtin: %v", err)
	}
	// Children inherit (and only narrow) the limits; status shows them.
	s.must("restrict -time 1h restrict -time 2h -goroutines 4 sleep 30 &")
	var pid int
	waitFor(t, func() bool {
		for _, p := range s.env.Table.List() {
			if p.Name == "sleep" {
				pid = p.PID
				return true
			}
		}
		return false
	})
	if got := s.must(fmt.Sprintf("grep limits < proc:/%d/status", pid)); got != "limits\ttime 1h0m0s, goroutines 4\n" {
		t.Fatalf("inherited limits: %q", got)
	}
	s.must(fmt.Sprintf("kill %d", pid))
	for _, bad := range []string{"restrict -time x ls", "restrict -out 0 ls", "restrict -goroutines -1 ls", "restrict -mem 1Q ls"} {
		if _, err := s.run(bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

// A filter in the middle stops when its reader goes away, so an endless
// source upstream ends too.
func TestMiddleStageStops(t *testing.T) {
	s := newShell(t)
	if got := s.must("yes | grep y | head -n 3 | wc -l"); got != "3\n" {
		t.Fatalf("got %q", got)
	}
	waitFor(t, func() bool { return len(s.env.Table.List()) == 1 })
}
