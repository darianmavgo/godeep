// Package pl011 reads input from an ARM PrimeCell UART (PL011) by polling,
// for boards whose console output goes to the UART some other way (e.g.
// board/applem1's printk mirror on QEMU). It only touches the receive side:
// the flag register and data-register reads, plus enabling the receiver.
package pl011

import (
	"context"
	"fmt"
	"sync"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// Registers and bits.
const (
	DR = 0x00 // data
	FR = 0x18 // flags
	CR = 0x30 // control

	FR_RXFE = 1 << 4 // receive FIFO empty

	CR_UARTEN = 1 << 0
	CR_TXE    = 1 << 8
	CR_RXE    = 1 << 9
)

// PollInterval is how often the receive FIFO is checked. QEMU's PL011 holds
// up to 32 bytes, so a paste longer than that within one interval may lose
// the rest; typing never comes close.
const PollInterval = 10 * time.Millisecond

// Compatible is the device-tree string.
const Compatible = "arm,pl011"

// Driver returns the input driver. The node's Config must be the
// chan<- byte that receives the input; bytes that don't fit are dropped.
func Driver(regs func(base uint64) mmio.Regs) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		out, ok := d.Config.(chan<- byte)
		if !ok {
			return fmt.Errorf("pl011: no input channel from the board (%T)", d.Config)
		}
		if len(d.Reg) == 0 {
			return fmt.Errorf("pl011: no registers")
		}
		r := regs(d.Reg[0].Base)
		if cr := r.Read32(CR); cr&(CR_UARTEN|CR_TXE|CR_RXE) != CR_UARTEN|CR_TXE|CR_RXE {
			r.Write32(CR, cr|CR_UARTEN|CR_TXE|CR_RXE)
		}
		st := &stats{}
		d.Ready(st)
		t := time.NewTicker(PollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
			for r.Read32(FR)&FR_RXFE == 0 {
				b := byte(r.Read32(DR))
				select {
				case out <- b:
					st.add(1, 0)
				default:
					st.add(0, 1)
				}
			}
		}
	}}
}

type stats struct {
	mu        sync.Mutex
	n, missed int
}

func (s *stats) add(n, missed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n += n
	s.missed += missed
}

func (s *stats) Devices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []string{fmt.Sprintf("console input (polled): %d bytes, %d dropped", s.n, s.missed)}
}
