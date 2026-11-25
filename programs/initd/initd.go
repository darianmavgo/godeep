// Package initd is PID 1: it supervises the shell and respawns it.
package initd

import (
	"context"
	"fmt"
	"time"

	"godeep/kernel/proc"
	"godeep/programs/sh"
)

func Main(env sh.Env) proc.Main {
	return func(ctx context.Context, p *proc.Proc) error {
		for {
			s := env.Table.Spawn(ctx, p.PID, proc.Spec{Name: "sh", Stdout: env.TTY, Main: sh.Main(env)})
			err := s.Wait(ctx)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if env.TTY.Closed() {
				return nil // console gone: shut down
			}
			if err != nil {
				fmt.Fprintf(env.TTY, "init: sh (pid %d) exited: %v; respawning\n", s.PID, err)
			} else {
				fmt.Fprintf(env.TTY, "init: sh (pid %d) exited; respawning\n", s.PID)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}
