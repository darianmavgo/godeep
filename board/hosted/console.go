//go:build !tamago

// Package hosted provides host-OS shims so the kernel runs as a normal process.
package hosted

import (
	"bufio"
	"os"
	"os/signal"
	"syscall"

	"godeep/kernel"
	"godeep/kernel/proc"
	"godeep/kernel/tty"
	"godeep/net/hostnet"
)

// NewConsole returns the host terminal as a console. The terminal stays in
// cooked mode (it echoes and edits lines itself); SIGINT is delivered as ^C.
func NewConsole() kernel.Console {
	in := make(chan byte, 256)
	stdin := make(chan byte)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	proc.Kthread("stdin", func() {
		r := bufio.NewReader(os.Stdin)
		for {
			b, err := r.ReadByte()
			if err != nil {
				close(stdin)
				return
			}
			stdin <- b
		}
	})
	// One driver goroutine owns `in`, merging keyboard bytes and signals.
	proc.Kthread("console", func() {
		for {
			select {
			case b, ok := <-stdin:
				if !ok {
					close(in)
					return
				}
				in <- b
			case <-sig:
				in <- tty.CtrlC
			}
		}
	})
	return kernel.Console{In: in, Out: os.Stdout, Echo: false}
}

// NewBoard returns the host as a board: the terminal is the console, and
// SIGTERM/SIGHUP (e.g. `kill <pid>`, closing the terminal) is a power-off
// request, so godeep shuts its processes down cleanly before exiting.
func NewBoard() kernel.Board {
	shut := make(chan struct{})
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGHUP)
	proc.Kthread("power", func() {
		<-term
		close(shut)
	})
	return kernel.Board{Console: NewConsole(), Shutdown: shut, Net: hostnet.Net{}, PowerOff: func() { os.Exit(0) }}
}
