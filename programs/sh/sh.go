// Package sh is the godeep shell.
package sh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"godeep/kernel/proc"
	"godeep/kernel/prog"
	"godeep/kernel/tty"
)

// Env is what the shell is given. (Phase 2 replaces this with capabilities.)
type Env struct {
	Table *proc.Table
	TTY   *tty.TTY
	Boot  time.Time
}

type builtin struct {
	usage string
	run   func(ctx context.Context, env Env, p *proc.Proc, args []string) error
}

var builtins map[string]builtin

func init() {
	builtins = map[string]builtin{
		"help":   {"list commands", help},
		"ps":     {"ps [-g] [PID]: processes, or goroutines with -g", ps},
		"kill":   {"kill PID...: cancel processes", kill},
		"uptime": {"time since boot", uptime},
		"mem":    {"memory and scheduler stats", mem},
		"halt":   {"power off", func(context.Context, Env, *proc.Proc, []string) error { os.Exit(0); return nil }},
		"exit":   {"exit this shell (init starts a new one)", nil},
	}
}

// Main returns the shell program.
func Main(env Env) proc.Main {
	return func(ctx context.Context, p *proc.Proc) error {
		t := env.TTY
		fmt.Fprintln(t, "type 'help' for commands")
		for {
			fmt.Fprint(t, "godeep$ ")
			line, err := t.ReadLine(ctx)
			if errors.Is(err, tty.ErrInterrupt) {
				continue
			}
			if err != nil {
				return err
			}
			args := strings.Fields(line)
			if len(args) == 0 {
				continue
			}
			if args[0] == "exit" {
				return nil
			}
			if err := run(ctx, env, p, args); err != nil {
				fmt.Fprintf(t, "%s: %v\n", args[0], err)
			}
		}
	}
}

func run(ctx context.Context, env Env, p *proc.Proc, args []string) error {
	bg := false
	if args[len(args)-1] == "&" {
		bg, args = true, args[:len(args)-1]
		if len(args) == 0 {
			return nil
		}
	} else if strings.HasSuffix(args[len(args)-1], "&") {
		bg = true
		args[len(args)-1] = strings.TrimSuffix(args[len(args)-1], "&")
	}
	if b, ok := builtins[args[0]]; ok {
		return b.run(ctx, env, p, args[1:])
	}
	pg, ok := prog.Lookup(args[0])
	if !ok {
		return errors.New("command not found")
	}
	child := env.Table.Spawn(ctx, p.PID, proc.Spec{Name: pg.Name, Args: args[1:], Stdout: env.TTY, Main: pg.Main})
	if bg {
		fmt.Fprintf(env.TTY, "[%d] %s\n", child.PID, pg.Name)
		return nil
	}
	// Foreground: wait, and turn ^C on the console into a kill.
	for {
		select {
		case <-child.Done():
			if err := child.Err(); err != nil && !errors.Is(err, proc.ErrKilled) {
				return err
			}
			return nil
		case <-env.TTY.Interrupts():
			child.Kill()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func help(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	w := table(env.TTY)
	fmt.Fprintln(w, "BUILTIN\tDESCRIPTION")
	for _, n := range []string{"help", "ps", "kill", "uptime", "mem", "exit", "halt"} {
		fmt.Fprintf(w, "%s\t%s\n", n, builtins[n].usage)
	}
	fmt.Fprintln(w, "\t")
	fmt.Fprintln(w, "PROGRAM\tUSAGE")
	for _, pg := range prog.All() {
		fmt.Fprintf(w, "%s\t%s\n", pg.Name, pg.Usage)
	}
	w.Flush()
	fmt.Fprintln(env.TTY, "\nappend & to run in the background; ^C kills the foreground program")
	return nil
}

func kill(_ context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kill PID...")
	}
	for _, a := range args {
		pid, err := strconv.Atoi(a)
		if err != nil {
			return fmt.Errorf("bad pid %q", a)
		}
		if err := env.Table.Kill(pid); err != nil {
			return err
		}
	}
	return nil
}

func uptime(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	fmt.Fprintf(env.TTY, "up %s\n", fmtDur(time.Since(env.Boot)))
	return nil
}

func mem(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w := table(env.TTY)
	fmt.Fprintf(w, "heap in use\t%s\n", fmtBytes(m.HeapInuse))
	fmt.Fprintf(w, "heap objects\t%d\n", m.HeapObjects)
	fmt.Fprintf(w, "total from system\t%s\n", fmtBytes(m.Sys))
	fmt.Fprintf(w, "GC cycles\t%d\n", m.NumGC)
	fmt.Fprintf(w, "goroutines\t%d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "GOMAXPROCS\t%d\n", runtime.GOMAXPROCS(0))
	return w.Flush()
}

func fmtBytes(b uint64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return d.Truncate(100 * time.Millisecond).String()
	}
	return d.Truncate(time.Second).String()
}
