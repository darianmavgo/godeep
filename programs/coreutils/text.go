package coreutils

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"godeep/kernel/proc"
	"godeep/kernel/prog"
)

// Text filters: they read stdin (a pipe, a < file, or the terminal until
// ^D) and write stdout, so they compose: ps | grep sh | wc -l.

func init() {
	prog.Register(prog.Program{Name: "grep", Usage: "grep [-v] [-i] [-c] REGEXP: lines of stdin that match", Main: grep})
	prog.Register(prog.Program{Name: "wc", Usage: "wc [-l]: count lines, words, bytes of stdin", Main: wc})
	prog.Register(prog.Program{Name: "head", Usage: "head [-n N | -N]: the first N lines of stdin (10)", Main: head})
	prog.Register(prog.Program{Name: "tail", Usage: "tail [-n N | -N]: the last N lines of stdin (10)", Main: tail})
	prog.Register(prog.Program{Name: "sort", Usage: "sort [-r] [-n]: sort the lines of stdin", Main: sortProg})
	prog.Register(prog.Program{Name: "yes", Usage: "yes [TEXT]: print TEXT (y) forever, until the reader stops", Main: yes})
}

// lines calls fn for each line of r (without its newline) until fn returns
// false, ctx is done or input ends.
func lines(ctx context.Context, r io.Reader, fn func(string) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !fn(sc.Text()) {
			return nil
		}
	}
	return sc.Err()
}

func grep(ctx context.Context, p *proc.Proc) error {
	invert, fold, count := false, false, false
	args := p.Args
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && len(args[0]) > 1 {
		for _, c := range args[0][1:] {
			switch c {
			case 'v':
				invert = true
			case 'i':
				fold = true
			case 'c':
				count = true
			default:
				return fmt.Errorf("unknown flag -%c", c)
			}
		}
		args = args[1:]
	}
	if len(args) != 1 {
		return errors.New("usage: grep [-v] [-i] [-c] REGEXP")
	}
	pat := args[0]
	if fold {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return err
	}
	n := 0
	w := bufio.NewWriter(p.Stdout)
	err = lines(ctx, p.Stdin, func(l string) bool {
		if re.MatchString(l) != invert {
			n++
			if !count {
				w.WriteString(l)
				w.WriteByte('\n')
			}
		}
		return true
	})
	if count {
		fmt.Fprintln(w, n)
	}
	if ferr := w.Flush(); err == nil {
		err = ferr
	}
	return err
}

func wc(ctx context.Context, p *proc.Proc) error {
	onlyLines := len(p.Args) == 1 && p.Args[0] == "-l"
	if len(p.Args) > 0 && !onlyLines {
		return errors.New("usage: wc [-l]")
	}
	var nl, nw, nb int
	buf := make([]byte, 32<<10)
	inWord := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := p.Stdin.Read(buf)
		for _, c := range buf[:n] {
			nb++
			if c == '\n' {
				nl++
			}
			space := c == ' ' || c == '\n' || c == '\t' || c == '\r'
			if !space && !inWord {
				nw++
			}
			inWord = !space
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if onlyLines {
		_, err := fmt.Fprintln(p.Stdout, nl)
		return err
	}
	_, err := fmt.Fprintf(p.Stdout, "%d %d %d\n", nl, nw, nb)
	return err
}

// count parses -n N or -N (default 10).
func count(args []string) (int, error) {
	switch {
	case len(args) == 0:
		return 10, nil
	case len(args) == 2 && args[0] == "-n":
		return strconv.Atoi(args[1])
	case len(args) == 1 && strings.HasPrefix(args[0], "-"):
		return strconv.Atoi(args[0][1:])
	case len(args) == 1:
		return strconv.Atoi(args[0])
	}
	return 0, errors.New("usage: [-n N | -N]")
}

func head(ctx context.Context, p *proc.Proc) error {
	n, err := count(p.Args)
	if err != nil || n < 0 {
		return errors.New("usage: head [-n N | -N]")
	}
	if n == 0 {
		return nil
	}
	i := 0
	var werr error
	err = lines(ctx, p.Stdin, func(l string) bool {
		_, werr = fmt.Fprintln(p.Stdout, l)
		i++
		return i < n && werr == nil
	})
	if werr != nil {
		return werr
	}
	return err // returning closes stdin: the writer upstream stops
}

func tail(ctx context.Context, p *proc.Proc) error {
	n, err := count(p.Args)
	if err != nil || n < 0 {
		return errors.New("usage: tail [-n N | -N]")
	}
	ring := make([]string, 0, n)
	err = lines(ctx, p.Stdin, func(l string) bool {
		if n == 0 {
			return true
		}
		if len(ring) == n {
			ring = append(ring[1:], l)
		} else {
			ring = append(ring, l)
		}
		return true
	})
	if err != nil {
		return err
	}
	for _, l := range ring {
		if _, err := fmt.Fprintln(p.Stdout, l); err != nil {
			return err
		}
	}
	return nil
}

func sortProg(ctx context.Context, p *proc.Proc) error {
	reverse, numeric := false, false
	for _, a := range p.Args {
		switch a {
		case "-r":
			reverse = true
		case "-n":
			numeric = true
		case "-rn", "-nr":
			reverse, numeric = true, true
		default:
			return errors.New("usage: sort [-r] [-n]")
		}
	}
	var all []string
	if err := lines(ctx, p.Stdin, func(l string) bool { all = append(all, l); return true }); err != nil {
		return err
	}
	less := func(a, b string) bool { return a < b }
	if numeric {
		num := func(s string) float64 {
			f := strings.Fields(s)
			if len(f) == 0 {
				return 0
			}
			v, _ := strconv.ParseFloat(f[0], 64)
			return v
		}
		less = func(a, b string) bool { return num(a) < num(b) }
	}
	sort.SliceStable(all, func(i, j int) bool {
		if reverse {
			return less(all[j], all[i])
		}
		return less(all[i], all[j])
	})
	w := bufio.NewWriter(p.Stdout)
	for _, l := range all {
		w.WriteString(l)
		w.WriteByte('\n')
	}
	return w.Flush()
}

func yes(ctx context.Context, p *proc.Proc) error {
	s := "y"
	if len(p.Args) > 0 {
		s = strings.Join(p.Args, " ")
	}
	// A whole buffer of lines per write (like GNU yes): one 2-byte write
	// per line would make every line a hand-off to the reader, which on
	// several cores means a cross-core wake-up each time.
	line := s + "\n"
	buf := []byte(strings.Repeat(line, max(1, 8192/len(line))))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := p.Stdout.Write(buf); err != nil {
			return err // the reader went away: the normal end
		}
	}
}
