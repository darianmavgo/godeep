//go:build tamago && arm64

package virt

import (
	"time"

	"godeep/kernel"
	"godeep/kernel/proc"
)

// NewConsole returns the PL011 as the console. A kernel thread (pid 0,
// "uart0") owns the UART and turns received bytes into channel sends. It
// polls; IRQ-driven RX needs the GIC (a later step).
func NewConsole() kernel.Console {
	in := make(chan byte, 256)
	proc.Kthread("uart0", func() {
		for {
			if c, ok := UART0.Rx(); ok {
				in <- c
				continue
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	return kernel.Console{In: in, Out: crlf{}, Echo: true}
}

// crlf writes to the UART, translating \n to \r\n for raw serial terminals.
type crlf struct{}

func (crlf) Write(p []byte) (int, error) {
	for _, c := range p {
		if c == '\n' {
			UART0.Tx('\r')
		}
		UART0.Tx(c)
	}
	return len(p), nil
}
