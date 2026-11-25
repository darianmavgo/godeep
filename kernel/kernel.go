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

const Version = "0.0.3"

// How long processes get to exit after a shutdown request.
const shutdownGrace = 2 * time.Second

// Console is what a board provides: an input byte stream (fed by a driver
// goroutine) and an output writer.
type Console struct {
	In   <-chan byte
	Out  io.Writer
	Echo bool // the kernel must echo input (raw serial)
}

// Board is everything the kernel needs from the platform.
type Board struct {
	Console Console
	// Shutdown receives when the host asks godeep to power off: the VM's
	// power button (QEMU system_powerdown) or SIGTERM for the hosted build.
	// nil if the board has no such signal.
	Shutdown <-chan struct{}
	// PowerOff is called after a clean shutdown. nil: Boot just returns.
	PowerOff func()
}

// Boot starts PID 1 and blocks until it exits, ctx is done, or the board
// requests a shutdown. On shutdown every process is killed, given
// shutdownGrace to exit, and stragglers are reported.
func Boot(ctx context.Context, b Board) error {
	con := b.Console
	fmt.Fprintf(con.Out, "godeep %s (%s/%s, %s, GOMAXPROCS=%d)\n",
		Version, runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.GOMAXPROCS(0))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	env := sh.Env{Table: proc.NewTable(), TTY: tty.New(con.In, con.Out, con.Echo), Boot: time.Now()}
	init := env.Table.Spawn(ctx, 0, proc.Spec{Name: "init", Stdout: env.TTY, Main: initd.Main(env)})

	var shutdown bool
	select {
	case <-init.Done():
	case <-ctx.Done():
	case <-b.Shutdown: // nil channel: never fires
		shutdown = true
		fmt.Fprintln(env.TTY, "\ngodeep: power-off requested, stopping all processes")
	}
	cancel()

	grace, done := context.WithTimeout(context.Background(), shutdownGrace)
	defer done()
	for _, p := range env.Table.Shutdown(grace) {
		fmt.Fprintf(env.TTY, "godeep: pid %d (%s) did not exit: %s\n", p.PID, p.Name, p.State())
	}
	if shutdown {
		fmt.Fprintln(env.TTY, "godeep: halted")
		if b.PowerOff != nil {
			b.PowerOff()
		}
	}
	if err := init.Err(); err != nil && err != proc.ErrKilled && err != context.Canceled {
		return err
	}
	return nil
}
