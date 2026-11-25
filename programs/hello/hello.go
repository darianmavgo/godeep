// Package hello is a tutorial program: a producer goroutine counts down
// over a channel while main prints what it receives.
package hello

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"godeep/kernel/proc"
	"godeep/kernel/prog"
)

func init() {
	prog.Register(prog.Program{
		Name:  "countdown",
		Usage: "countdown [N]: count down from N using two goroutines",
		Main:  countdown,
	})
}

func countdown(ctx context.Context, p *proc.Proc) error {
	n := 5
	if len(p.Args) > 0 {
		if v, err := strconv.Atoi(p.Args[0]); err == nil {
			n = v
		}
	}

	ticks := make(chan int)

	// A second goroutine in this process. p.Go (never a bare `go`) makes
	// it count toward this PID and contains its panics.
	p.Go(func(ctx context.Context) {
		defer close(ticks)
		for i := n; i > 0; i-- {
			select {
			case ticks <- i:
			case <-ctx.Done(): // killed: stop promptly
				return
			}
			time.Sleep(time.Second)
		}
	})

	for i := range ticks {
		fmt.Fprintf(p.Stdout, "%d...\n", i)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fmt.Fprintln(p.Stdout, "liftoff!")
	return nil
}
