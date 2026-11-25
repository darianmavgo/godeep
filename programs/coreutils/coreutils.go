// Package coreutils holds small demo programs. They register at init().
package coreutils

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"godeep/kernel/proc"
	"godeep/kernel/prog"
)

func init() {
	prog.Register(prog.Program{Name: "echo", Usage: "echo ARGS...", Main: echo})
	prog.Register(prog.Program{Name: "sleep", Usage: "sleep SECONDS", Main: sleep})
	prog.Register(prog.Program{Name: "workers", Usage: "workers [N]: N goroutines doing periodic work until killed", Main: workers})
	prog.Register(prog.Program{Name: "pingpong", Usage: "pingpong [N]: two goroutines pass a ball over a channel", Main: pingpong})
	prog.Register(prog.Program{Name: "leak", Usage: "leak: start a goroutine that ignores kill (cooperative-kill demo)", Main: leak})
}

func argInt(p *proc.Proc, def int) int {
	if len(p.Args) > 0 {
		if n, err := strconv.Atoi(p.Args[0]); err == nil {
			return n
		}
	}
	return def
}

func echo(_ context.Context, p *proc.Proc) error {
	fmt.Fprintln(p.Stdout, strings.Join(p.Args, " "))
	return nil
}

func sleep(ctx context.Context, p *proc.Proc) error {
	secs := 1.0
	if len(p.Args) > 0 {
		f, err := strconv.ParseFloat(p.Args[0], 64)
		if err != nil {
			return fmt.Errorf("bad duration %q", p.Args[0])
		}
		secs = f
	}
	select {
	case <-time.After(time.Duration(secs * float64(time.Second))):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func workers(ctx context.Context, p *proc.Proc) error {
	n := argInt(p, 4)
	results := make(chan int)
	for i := 0; i < n; i++ {
		p.Go(func(ctx context.Context) {
			t := time.NewTicker(time.Duration(200+100*i) * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					select {
					case results <- i:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		})
	}
	// main goroutine: collects results (the process "reduce" step)
	count := make([]int, n)
	for {
		select {
		case i := <-results:
			count[i]++
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func pingpong(ctx context.Context, p *proc.Proc) error {
	n := argInt(p, 5)
	ping, pong := make(chan int), make(chan int)
	p.Go(func(ctx context.Context) {
		for {
			select {
			case b := <-ping:
				fmt.Fprintf(p.Stdout, "  pong %d\n", b)
				time.Sleep(300 * time.Millisecond)
				select {
				case pong <- b + 1:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	})
	for b := 0; b < n; {
		fmt.Fprintf(p.Stdout, "ping %d\n", b)
		time.Sleep(300 * time.Millisecond)
		select {
		case ping <- b:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case b = <-pong:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// leak deliberately breaks the rules: a bare `go` whose goroutine ignores its
// context. It still carries the proc's pprof labels, so after the process
// exits, `ps` shows it as orphaned.
func leak(_ context.Context, p *proc.Proc) error {
	go func() { //nolint: intentional rule violation for the demo
		for {
			time.Sleep(time.Hour)
		}
	}()
	fmt.Fprintf(p.Stdout, "pid %d exits now; its goroutine keeps running (see ps)\n", p.PID)
	return nil
}
