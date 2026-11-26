// Package initd is PID 1: it starts sshd (if there is a network) and
// supervises the console shell, respawning it.
package initd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"godeep/kernel/cap"
	"godeep/kernel/proc"
	"godeep/programs/sh"
	"godeep/programs/sshd"
)

func Main(env sh.Env, version string) proc.Main {
	return func(ctx context.Context, p *proc.Proc) error {
		if env.Net != nil {
			var log io.Writer = env.TTY
			if env.Log != nil {
				log = env.Log
			}
			cfg := sshd.Config{Net: env.Net, Shell: env, Version: version, Log: log}
			env.Table.Spawn(ctx, p.PID, proc.Spec{Name: "sshd", Stdout: env.TTY, Main: func(ctx context.Context, p *proc.Proc) error {
				err := sshd.Main(cfg)(ctx, p)
				if errors.Is(err, cap.ErrNoNetwork) {
					return nil // nothing to serve on: exit quietly
				}
				if err != nil && ctx.Err() == nil {
					fmt.Fprintf(env.TTY, "sshd: %v\n", err)
				}
				return err
			}})
		}
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
