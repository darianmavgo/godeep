// Package kernel boots godeep. It must never import board/*.
package kernel

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"time"

	"godeep/kernel/proc"
	"godeep/kernel/tty"
	_ "godeep/programs/coreutils"
	_ "godeep/programs/hello"
	"godeep/programs/initd"
	"godeep/programs/sh"
)

const Version = "0.0.2"

// Console is what a board provides: an input byte stream (fed by a driver
// goroutine) and an output writer.
type Console struct {
	In   <-chan byte
	Out  io.Writer
	Echo bool // the kernel must echo input (raw serial)
}

// Boot starts PID 1 and blocks until it exits or ctx is done.
func Boot(ctx context.Context, con Console) error {
	fmt.Fprintf(con.Out, "godeep %s (%s/%s, %s, GOMAXPROCS=%d)\n",
		Version, runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.GOMAXPROCS(0))
	env := sh.Env{Table: proc.NewTable(), TTY: tty.New(con.In, con.Out, con.Echo), Boot: time.Now()}
	init := env.Table.Spawn(ctx, 0, proc.Spec{Name: "init", Stdout: env.TTY, Main: initd.Main(env)})
	return init.Wait(ctx)
}
