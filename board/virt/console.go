//go:build tamago && arm64 && !applem1

package virt

import (
	"time"

	"godeep/kernel"
	"godeep/kernel/irq"
	"godeep/kernel/proc"
)

func read_midr() uint64

// OnAppleHVF reports whether the vCPU is an Apple core, i.e. QEMU is running
// us under Hypervisor.framework (-accel hvf -cpu host).
func OnAppleHVF() bool { return read_midr()>>24&0xff == 0x61 }

// UARTMode is how the console receives input: "irq" or "poll".
//
// Under HVF, QEMU 11.1.x has a WFI bug (QEMU issue 959, "Fix WFI halting to
// stop idle vCPU spinning"): after the first device interrupt, every WFI
// returns immediately and an idle VM burns a full host core, although the
// guest has nothing pending (ISR_EL1=0, GIC idle, timer armed). Polling the
// UART avoids delivering a device interrupt during normal use. Interrupt
// mode is used everywhere else (TCG, future hosts and hardware).
var UARTMode = "irq"

// NewConsole returns the PL011 as the console. A kernel thread (pid 0,
// "uart0") owns the UART and turns received bytes into channel sends.
func NewConsole(b *irq.Bridge) kernel.Console {
	in := make(chan byte, 256)
	if OnAppleHVF() {
		UARTMode = "poll"
	}
	if UARTMode == "poll" {
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

	// Interrupt mode: sleep until the receive interrupt, drain the FIFO,
	// then re-arm.
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
