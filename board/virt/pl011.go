//go:build tamago && arm64

package virt

import (
	"sync/atomic"
	"unsafe"
)

// ARM PrimeCell UART (PL011) registers.
const (
	uartDR   = 0x00 // data
	uartFR   = 0x18 // flags
	uartCR   = 0x30 // control
	uartIMSC = 0x38 // interrupt mask set/clear
	uartICR  = 0x44 // interrupt clear

	intRX = 1 << 4 // receive
	intRT = 1 << 6 // receive timeout

	frRXFE = 1 << 4 // receive FIFO empty
	frTXFF = 1 << 5 // transmit FIFO full

	crUARTEN = 1 << 0
	crTXE    = 1 << 8
	crRXE    = 1 << 9
)

// PL011 is a minimal polled PL011 driver. Accesses are single 32-bit
// loads/stores so they trap cleanly under HVF.
type PL011 struct {
	Base uintptr
}

func (u *PL011) reg(off uintptr) *uint32 {
	return (*uint32)(unsafe.Pointer(u.Base + off))
}

// Init enables the UART (QEMU's PL011 only transmits when UARTEN and TXE are set).
func (u *PL011) Init() {
	atomic.StoreUint32(u.reg(uartCR), crUARTEN|crTXE|crRXE)
}

// EnableRxInterrupt makes the UART raise its interrupt when data arrives.
func (u *PL011) EnableRxInterrupt() {
	atomic.StoreUint32(u.reg(uartIMSC), intRX|intRT)
}

// ClearRxInterrupt acknowledges the receive interrupt at the UART.
func (u *PL011) ClearRxInterrupt() {
	atomic.StoreUint32(u.reg(uartICR), intRX|intRT)
}

// Tx transmits one byte.
func (u *PL011) Tx(c byte) {
	for atomic.LoadUint32(u.reg(uartFR))&frTXFF != 0 {
	}
	atomic.StoreUint32(u.reg(uartDR), uint32(c))
}

// Rx returns a received byte, if any (non-blocking).
func (u *PL011) Rx() (byte, bool) {
	if atomic.LoadUint32(u.reg(uartFR))&frRXFE != 0 {
		return 0, false
	}
	return byte(atomic.LoadUint32(u.reg(uartDR))), true
}

// printk is where the Go runtime sends print/println/panic output.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	UART0.Tx(c)
}
