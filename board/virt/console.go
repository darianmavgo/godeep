//go:build tamago && arm64

package virt

import (
	"godeep/kernel"
	"godeep/kernel/irq"
	"godeep/kernel/proc"
)

// NewConsole returns the PL011 as the console. A kernel thread (pid 0,
// "uart0") owns the UART: it sleeps until the UART's receive interrupt,
// drains the FIFO into a channel, then re-arms the interrupt.
func NewConsole(b *irq.Bridge) kernel.Console {
	in := make(chan byte, 256)
	line, err := b.Request(UART0_IRQ, "uart0")
	if err != nil {
		panic(err)
	}
	UART0.EnableRxInterrupt()
	proc.Kthread("uart0", func() {
		for {
			for {
				c, ok := UART0.Rx()
				if !ok {
					break
				}
				in <- c
			}
			UART0.ClearRxInterrupt()
			line.Done()
			<-line.C
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
