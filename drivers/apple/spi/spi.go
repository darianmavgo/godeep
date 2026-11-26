// Package spi drives the Apple Silicon SPI controller ("apple,spi"), after
// Linux's spi-apple.c and U-Boot's apple_spi.c. Transfers are polled
// (programmed I/O through 16-entry FIFOs, 8-bit words), like U-Boot's.
package spi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"godeep/kernel/mmio"
)

// Registers.
const (
	CTRL       = 0x000
	CFG        = 0x004
	STATUS     = 0x008
	PIN        = 0x00c
	TXDATA     = 0x010
	RXDATA     = 0x020
	CLKDIV     = 0x030
	RXCNT      = 0x034
	TXCNT      = 0x04c
	FIFOSTAT   = 0x10c
	IE_XFER    = 0x130
	IF_XFER    = 0x134
	IE_FIFO    = 0x138
	IF_FIFO    = 0x13c
	SHIFTCFG   = 0x150
	PINCFG     = 0x154
	DELAY_PRE  = 0x160
	DELAY_POST = 0x168

	CTRL_RUN      = 1 << 0
	CTRL_TX_RESET = 1 << 2
	CTRL_RX_RESET = 1 << 3

	CFG_CPHA           = 1 << 1
	CFG_CPOL           = 1 << 2
	CFG_MODE_IRQ       = 1 << 5 // MODE field (bits 6:5) = 1, as Linux and U-Boot set it
	CFG_LSB_FIRST      = 1 << 13
	CFG_WORD_SIZE_8B   = 0 << 15
	CFG_FIFO_THRESH_8B = 0 << 17

	XFER_TXCOMPLETE = 1 << 1 // IF_XFER: the last byte has been shifted out

	PIN_CS = 1 << 1 // 1 = CS high (inactive); CS is active low

	SHIFTCFG_OVERRIDE_CS = 1 << 24

	PINCFG_KEEP_CS     = 1 << 1
	PINCFG_CS_IDLE_VAL = 1 << 9

	FIFOSTAT_LEVEL_TX = 0xff << 8
	FIFOSTAT_LEVEL_RX = 0xff << 24

	CLKDIV_MIN = 0x002
	CLKDIV_MAX = 0x7ff

	FifoDepth = 16
)

// Timeout for one transfer.
const Timeout = 200 * time.Millisecond

// Controller is one SPI controller.
type Controller struct{ R mmio.Regs }

// Init puts the controller in a known state (Linux apple_spi_init): CS
// inactive and under software control, FIFOs reset, 8-bit words, mode 0,
// interrupts and delays off.
func (c Controller) Init() {
	c.R.Write32(PIN, PIN_CS)
	c.R.Write32(SHIFTCFG, c.R.Read32(SHIFTCFG)&^SHIFTCFG_OVERRIDE_CS)
	c.R.Write32(PINCFG, c.R.Read32(PINCFG)&^PINCFG_CS_IDLE_VAL|PINCFG_KEEP_CS)
	c.R.Write32(CTRL, CTRL_RX_RESET|CTRL_TX_RESET)
	c.R.Write32(CFG, CFG_FIFO_THRESH_8B|CFG_MODE_IRQ|CFG_WORD_SIZE_8B)
	c.R.Write32(IE_FIFO, 0)
	c.R.Write32(IE_XFER, 0)
	c.R.Write32(DELAY_PRE, 0)
	c.R.Write32(DELAY_POST, 0)
}

// SetSpeed sets the bit clock to at most hz from the controller's input
// clock, and returns the rate it got.
func (c Controller) SetSpeed(clockHz, hz uint64) uint64 {
	div := (clockHz + hz - 1) / hz
	div = max(CLKDIV_MIN, min(div, CLKDIV_MAX))
	c.R.Write32(CLKDIV, uint32(div))
	return clockHz / div
}

// CS asserts (drives low) or releases chip select.
func (c Controller) CS(assert bool) {
	if assert {
		c.R.Write32(PIN, 0)
	} else {
		c.R.Write32(PIN, PIN_CS)
	}
}

// Transfer clocks len(tx) bytes out and/or len(rx) bytes in (one may be
// nil; if both are set they must be the same length). It busy-polls the
// FIFOs, so it holds the core for the whole transfer: 1 µs per byte at
// 8 MHz, ~0.26 ms for a 256-byte packet.
func (c Controller) Transfer(ctx context.Context, tx, rx []byte) error {
	if tx != nil && rx != nil && len(tx) != len(rx) {
		return errors.New("spi: tx and rx lengths differ")
	}
	c.R.Write32(CTRL, CTRL_RX_RESET|CTRL_TX_RESET)
	c.R.Write32(IF_XFER, ^uint32(0))
	c.R.Write32(IF_FIFO, ^uint32(0))
	c.R.Write32(TXCNT, uint32(len(tx)))
	c.R.Write32(RXCNT, uint32(len(rx)))
	ti, ri := 0, 0
	fill := func() {
		free := FifoDepth - int(c.R.Read32(FIFOSTAT)&FIFOSTAT_LEVEL_TX)>>8
		for ; free > 0 && ti < len(tx); free-- {
			c.R.Write32(TXDATA, uint32(tx[ti]))
			ti++
		}
	}
	fill()
	c.R.Write32(CTRL, CTRL_RUN)
	deadline := time.Now().Add(Timeout)
	for ti < len(tx) || ri < len(rx) {
		if n := int(c.R.Read32(FIFOSTAT)&FIFOSTAT_LEVEL_RX) >> 24; n > 0 {
			for ; n > 0 && ri < len(rx); n-- {
				rx[ri] = byte(c.R.Read32(RXDATA))
				ri++
			}
			continue
		}
		fill()
		if time.Now().After(deadline) {
			c.R.Write32(CTRL, 0)
			return fmt.Errorf("spi: timeout: sent %d/%d, received %d/%d", ti, len(tx), ri, len(rx))
		}
		if err := ctx.Err(); err != nil {
			c.R.Write32(CTRL, 0)
			return err
		}
	}
	// The TX FIFO may still be shifting out: wait before CS is released.
	for len(tx) > 0 && c.R.Read32(IF_XFER)&XFER_TXCOMPLETE == 0 {
		if time.Now().After(deadline) {
			c.R.Write32(CTRL, 0)
			return errors.New("spi: timeout waiting for the last byte to go out")
		}
	}
	return nil
}
