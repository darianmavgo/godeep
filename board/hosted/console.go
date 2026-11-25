//go:build !tamago

// Package hosted provides host-OS shims so the kernel runs as a normal process.
package hosted

import (
	"bufio"
	"os"
	"os/signal"

	"godeep/kernel"
	"godeep/kernel/proc"
	"godeep/kernel/tty"
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
