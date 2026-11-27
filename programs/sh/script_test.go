package sh

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLexVars(t *testing.T) {
	look := func(n string) string { return map[string]string{"A": "x y", "?": "3", "1": "one"}[n] }
	for _, c := range []struct {
		line string
		want []string
	}{
		{"echo $A", []string{"echo", "x y"}},
		{`echo "<$A>" '$A' \$A ${A}b $Ab`, []string{"echo", "<x y>", "$A", "$A", "x yb", ""}},
		{"echo $? $1 $ $#", []string{"echo", "3", "one", "$", ""}},
		{`echo "a\$b" a\ b`, []string{"echo", "a$b", "a b"}},
		{"echo a#b # comment", []string{"echo", "a#b"}},
		{"# only a comment", nil},
		{`echo "$"`, []string{"echo", "$"}},
	} {
		toks, err := lex(c.line)
		if err != nil {
			t.Errorf("%q: %v", c.line, err)
			continue
		}
		var got []string
		for _, tk := range toks {
			got = append(got, tk.word(look))
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %q, want %q", c.line, got, c.want)
		}
	}
	for _, bad := range []string{"echo ${A", "echo ${1x}", "a ;; b", "; a", "a &&", "a || ", "&"} {
		toks, err := lex(bad)
		if err == nil {
			_, err = splitItems(toks)
		}
		if err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestLists(t *testing.T) {
	s := newShell(t)
	if got := s.must("echo a; echo b ; echo c"); got != "a\nb\nc\n" {
		t.Fatalf("; %q", got)
	}
	if got := s.must("true && echo yes || echo no"); got != "yes\n" {
		t.Fatalf("&& ||: %q", got)
	}
	if got := s.must("false && echo yes || echo no"); got != "no\n" {
		t.Fatalf("false && ||: %q", got)
	}
	if got := s.must("false || true && echo ok"); got != "ok\n" {
		t.Fatalf("|| &&: %q", got)
	}
	// The last command's failure is returned; earlier ones are printed.
	got, err := s.run("cat ram:/nosuch; echo after")
	if err != nil || !strings.HasPrefix(got, "cat: ") || !strings.HasSuffix(got, "after\n") {
		t.Fatalf("error then echo: %q, %v", got, err)
	}
	_, err = s.run("echo x; cat ram:/nosuch")
	var ce *cmdError
	if !errors.As(err, &ce) || ce.name != "cat" {
		t.Fatalf("last error: %v", err)
	}
	if got := s.must("false; echo $?; true; echo $?"); got != "1\n0\n" {
		t.Fatalf("$?: %q", got)
	}
	if _, err := s.run("false"); exitStatus(err) != 1 {
		t.Fatalf("false: %v", err)
	}
}

func TestVariables(t *testing.T) {
	s := newShell(t)
	s.must("A=hello B='big world'")
	if got := s.must("echo $A, \"$B\"!"); got != "hello, big world!\n" {
		t.Fatalf("expand: %q", got)
	}
	s.must("F=ram:/v; echo $B > $F")
	if got := s.must("cat $F | wc"); got != "1 2 10\n" {
		t.Fatalf("redirect to $F: %q", got)
	}
	if got := s.must("set"); !strings.Contains(got, "A=hello\n") || !strings.Contains(got, "F=ram:/v\n") {
		t.Fatalf("set: %q", got)
	}
	s.must("unset A")
	if got := s.must("echo [$A]"); got != "[]\n" {
		t.Fatalf("unset: %q", got)
	}
}

func TestTest(t *testing.T) {
	s := newShell(t)
	s.must("echo hi > ram:/f; mkdir ram:/d")
	for line, want := range map[string]int{
		"test -e ram:/f": 0, "test -f ram:/f": 0, "test -d ram:/f": 1, "test -d ram:/d": 0,
		"test -e ram:/no": 1, "[ a = a ]": 0, "[ a != a ]": 1, "test 3 -lt 10": 0,
		"test 10 -le 3": 1, "test -z ''": 0, "test -n ''": 1, "test ! -e ram:/no": 0,
		"test x": 0, "test": 1, "[ 1 -eq x ]": 1, "[ a = a": 1,
	} {
		_, err := s.run(line)
		if got := exitStatus(err); got != want {
			t.Errorf("%s: status %d (%v), want %d", line, got, err, want)
		}
	}
}

func TestSource(t *testing.T) {
	s := newShell(t)
	script := `# greet
N=$1
echo hello $N, $# args
cat ram:/missing
test -e ram:/f || echo no file
echo $0 done
`
	s.must("write ram:/s.sh " + quoteAll(script))
	got := s.must("source ram:/s.sh you two")
	want := "hello you, 2 args\ncat: open missing: file does not exist\nno file\nram:/s.sh done\n"
	if !strings.HasPrefix(got, "hello you, 2 args\ncat: ") || !strings.HasSuffix(got, "no file\nram:/s.sh done\n") {
		t.Fatalf("source:\n%q\nwant like\n%q", got, want)
	}
	// Its variables are the shell's; $1 is restored afterwards.
	if got := s.must("echo $N [$1]"); got != "you []\n" {
		t.Fatalf("after source: %q", got)
	}
	// A script that sources itself stops at the depth limit.
	s.must("write ram:/loop.sh '. ram:/loop.sh'")
	if _, err := s.run(". ram:/loop.sh"); err == nil || !strings.Contains(err.Error(), "too deep") {
		t.Fatalf("recursion: %v", err)
	}
}

// quoteAll makes text one single-quoted word for write (no ' inside).
func quoteAll(s string) string { return "'" + s + "'" }

func TestJobs(t *testing.T) {
	s := newShell(t)
	out := s.must("sleep 30 &")
	if !strings.HasSuffix(out, "sleep (%1)\n") {
		t.Fatalf("bg: %q", out)
	}
	s.must("echo x | sleep 0.1 &")
	if got := s.must("jobs"); !strings.Contains(got, "%1\trunning") || !strings.Contains(got, "%2") || !strings.Contains(got, "sleep 30") {
		t.Fatalf("jobs:\n%s", got)
	}
	s.must("wait %2")
	if got := s.must("jobs"); strings.Contains(got, "%2") {
		t.Fatalf("job 2 still listed:\n%s", got)
	}
	if _, err := s.run("wait %9"); err == nil {
		t.Fatal("wait for a missing job")
	}
	// fg: ^C kills the job.
	done := make(chan error, 1)
	go func() { _, err := s.run("fg"); done <- err }()
	waitFor(t, func() bool { return strings.Contains(s.out.String(), "sleep 30\n") })
	// ^C reaches the waiter only while it listens: type it until it does.
	for stop := false; !stop; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("fg: %v", err)
			}
			stop = true
		case s.in <- 3:
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor(t, func() bool { return len(s.env.Table.List()) == 1 })
	if got := s.must("jobs"); got != "" {
		t.Fatalf("jobs after fg: %q", got)
	}
}

func FuzzLex(f *testing.F) {
	for _, s := range []string{"echo $A ${B} \"$?\" 'x' \\$ a#b # c", "a && b || c; d &", "${", "\"$", "\\"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		toks, err := lex(line)
		if err != nil {
			return
		}
		items, err := splitItems(toks)
		if err != nil {
			return
		}
		for _, it := range items {
			parsePipe(it.toks, func(string) string { return "v" })
		}
	})
}

func TestEditCmd(t *testing.T) {
	s := newShell(t)
	s.must("echo first line > ram:/e.txt")
	s.env.TTY.SetSize(40, 10)
	done := make(chan error, 1)
	go func() { _, err := s.run("edit ram:/e.txt"); done <- err }()
	waitFor(t, func() bool { return strings.Contains(s.out.String(), "\x1b[?25h") }) // drawn: raw input on
	// End of line, type, a new line, save, quit
	for _, b := range []byte("\x1b[F!\rsecond\x13\x11") {
		s.in <- b
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("edit didn't quit; screen:\n%q", s.out.String())
	}
	if got := s.must("cat ram:/e.txt"); got != "first line!\nsecond\n" {
		t.Fatalf("saved %q", got)
	}
	if out := s.out.String(); !strings.Contains(out, "\x1b[7m") || !strings.Contains(out, "saved 2 lines") {
		t.Fatalf("screen output lacks the status line: %q", out)
	}
	// the shell reads lines again afterwards
	if got := s.must("echo after"); got != "after\n" {
		t.Fatalf("after edit: %q", got)
	}
	if _, err := s.run("edit ram:/e.txt > ram:/x"); err == nil {
		t.Fatal("edit in a pipeline")
	}
}

func TestTimePrefix(t *testing.T) {
	s := newShell(t)
	out := s.must("time yes | head -1000 | wc -l")
	if !strings.HasPrefix(out, "1000\nreal ") {
		t.Fatalf("time: %q", out)
	}
	if _, err := s.run("time"); err == nil {
		t.Fatal("bare time")
	}
}
