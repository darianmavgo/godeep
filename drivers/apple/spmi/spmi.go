// Package spmi drives the M1 Pro's SPMI controller ("apple,t6000-spmi"),
// polled, after Linux's spmi-apple-controller.c: just extended register
// reads and writes, which is how the PMIC's NVMEM (the RTC offset, boot
// flags) is reached. One command at a time.
package spmi

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"context"
	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// Compatible is the controller in the M1 Pro's device tree.
const Compatible = "apple,t6000-spmi"

const (
	STATUS = 0x00
	CMD    = 0x04
	RSP    = 0x08
	ACT    = 0xa4

	RX_EMPTY   = 1 << 24
	FIFO_FLUSH = 1 << 0
	replyAck   = 1 << 15

	cmdExtWriteL = 0x30
	cmdExtReadL  = 0x38

	Timeout = 50 * time.Millisecond
)

// Bus is the controller's service.
type Bus struct {
	mu       sync.Mutex
	r        mmio.Regs
	prevFail bool
	reads    int
}

// New returns the bus over the controller's registers.
func New(r mmio.Regs) *Bus { return &Bus{r: r} }

func pack(opc, sid byte, param uint16) uint32 {
	return uint32(opc) | uint32(sid)<<8 | uint32(param)<<16 | 1<<15
}

func (b *Bus) waitRx() error {
	deadline := time.Now().Add(Timeout)
	for b.r.Read32(STATUS)&RX_EMPTY != 0 {
		if time.Now().After(deadline) {
			b.prevFail = true
			return errors.New("spmi: no reply")
		}
		time.Sleep(10 * time.Microsecond)
	}
	return nil
}

// Read reads n (1..8) bytes from register addr of slave sid (extended
// register read, long: 16-bit address).
func (b *Bus) Read(sid byte, addr uint16, n int) ([]byte, error) {
	if n < 1 || n > 8 {
		return nil, fmt.Errorf("spmi: read of %d bytes", n)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.prevFail {
		b.r.Write32(ACT, FIFO_FLUSH)
		b.prevFail = false
	}
	b.r.Write32(CMD, pack(cmdExtReadL|byte(n-1), sid, addr))
	if err := b.waitRx(); err != nil {
		return nil, err
	}
	reply := b.r.Read32(RSP)
	out := make([]byte, 0, n)
	for len(out) < n {
		if b.r.Read32(STATUS)&RX_EMPTY != 0 {
			b.prevFail = true
			return nil, errors.New("spmi: reply data missing")
		}
		w := b.r.Read32(RSP)
		for i := 0; i < 4 && len(out) < n; i++ {
			out = append(out, byte(w>>(8*i)))
		}
	}
	if b.r.Read32(STATUS)&RX_EMPTY == 0 {
		b.prevFail = true
	}
	if parity := reply >> 16; ^parity&(1<<n-1) != 0 {
		return nil, fmt.Errorf("spmi: parity error in reply %#x", reply)
	}
	b.reads++
	return out, nil
}

func (b *Bus) Devices() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return []string{fmt.Sprintf("polled, %d reads", b.reads)}
}

// Driver publishes the bus.
func Driver(regs func(base uint64) mmio.Regs) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		if len(d.Reg) == 0 {
			return errors.New("spmi: no registers")
		}
		d.Ready(New(regs(d.Reg[0].Base)))
		<-ctx.Done()
		return nil
	}}
}
