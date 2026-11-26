//go:build tamago && amd64

// Package microvm is the board glue for QEMU's amd64 microvm machine.
package microvm

import (
	"os"
	"time"

	vm "github.com/usbarmory/tamago/board/qemu/microvm"

	"godeep/kernel"
	"godeep/kernel/proc"
)

// NewConsole returns the COM1 serial port as the console. A kernel thread
// (pid 0, "uart0") owns the UART and turns received bytes into channel sends.
// It polls; IRQ-driven RX (UART0.EnableInterrupt + I/O APIC) is a later step.
func NewConsole() kernel.Console {
	in := make(chan byte, 256)
	proc.Kthread("uart0", func() {
		for {
			if c, ok := vm.UART0.Rx(false); ok {
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
			vm.UART0.Tx('\r')
		}
		vm.UART0.Tx(c)
	}
	return len(p), nil
}

// NewBoard returns the microvm board. Its power button is an ACPI event,
// which godeep does not handle yet, so there is no Shutdown channel: `make
// stop` falls back to killing QEMU.
func NewBoard() kernel.Board {
	return kernel.Board{Console: NewConsole(), PowerOff: func() { os.Exit(0) }} // triple fault: QEMU exits (-no-reboot)
}
