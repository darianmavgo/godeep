// Package i2c drives the M1 Pro's I2C controllers ("apple,t6000-i2c", a
// PA Semi design), polled, after m1n1's i2c.c. It does just what the USB-C
// port controllers (CD321x) need: TI-style register reads, where the device
// answers with a length byte and then the data. m1n1 leaves the controller
// clocked and configured; godeep keeps its CONTROL value.
//
// One goroutine owns the controller; the service (*Bus) sends it requests.
package i2c

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"godeep/drivers/apple/pmgr"
	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// Compatible is the controller in the M1 Pro's device tree.
const Compatible = "apple,t6000-i2c"

// Registers.
const (
	FIFO_TX = 0x00
	FIFO_RX = 0x04
	STATUS  = 0x14 // write 1 to clear
	CONTROL = 0x1c

	TX_READ          = 1 << 10
	TX_STOP          = 1 << 9
	TX_START         = 1 << 8
	RX_EMPTY         = 1 << 8
	STATUS_XFER_BUSY = 1 << 28
	STATUS_XFER_END  = 1 << 27
	CONTROL_CLEAR_RX = 1 << 10
	CONTROL_CLEAR_TX = 1 << 9

	Timeout = 50 * time.Millisecond
)

// Config is what the board resolves: the controller's power domains.
type Config struct {
	Power []pmgr.Domain
}

// Bus is the controller's service.
type Bus struct {
	reqs          chan *req
	reads, errors atomic.Uint64
}

type req struct {
	addr, reg byte
	n         int
	done      chan result
}

type result struct {
	b   []byte
	err error
}

// ReadReg reads register reg of the device at addr (7-bit): up to n data
// bytes after the device's length byte.
func (b *Bus) ReadReg(ctx context.Context, addr, reg byte, n int) ([]byte, error) {
	r := &req{addr: addr, reg: reg, n: n, done: make(chan result, 1)}
	select {
	case b.reqs <- r:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case res := <-r.done:
		return res.b, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *Bus) Devices() []string {
	return []string{fmt.Sprintf("polled, %d reads, %d errors", b.reads.Load(), b.errors.Load())}
}

// Driver returns the controller driver.
func Driver(regs func(base uint64) mmio.Regs) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		if len(d.Reg) == 0 {
			return errors.New("i2c: no registers")
		}
		if c, ok := d.Config.(Config); ok {
			for _, p := range c.Power {
				if _, err := pmgr.On(ctx, regs(p.Addr)); err != nil {
					return fmt.Errorf("i2c: power domain %s: %w", p.Name, err)
				}
			}
		}
		return Serve(ctx, d, regs(d.Reg[0].Base))
	}}
}

// Serve publishes the Bus and runs transfers until ctx is done.
func Serve(ctx context.Context, d *dev.Device, r mmio.Regs) error {
	b := &Bus{reqs: make(chan *req)}
	c := &ctl{r: r, control: r.Read32(CONTROL) &^ (CONTROL_CLEAR_RX | CONTROL_CLEAR_TX)}
	if d != nil {
		d.Ready(b)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case q := <-b.reqs:
			v, err := c.read(q.addr, q.reg, q.n)
			b.reads.Add(1)
			if err != nil {
				b.errors.Add(1)
			}
			q.done <- result{v, err}
		}
	}
}

type ctl struct {
	r       mmio.Regs
	control uint32
}

func (c *ctl) read(addr, reg byte, n int) ([]byte, error) {
	r := c.r
	r.Write32(CONTROL, c.control|CONTROL_CLEAR_RX|CONTROL_CLEAR_TX)
	r.Write32(STATUS, 0xffffffff)
	r.Write32(FIFO_TX, TX_START|uint32(addr)<<1)
	r.Write32(FIFO_TX, uint32(reg))
	r.Write32(FIFO_TX, TX_START|uint32(addr)<<1|1)
	r.Write32(FIFO_TX, TX_READ|TX_STOP|uint32(n+1))
	var out []byte
	length := -1
	for i := 0; i < n+1; i++ {
		deadline := time.Now().Add(Timeout)
		v := r.Read32(FIFO_RX)
		for v&RX_EMPTY != 0 {
			if time.Now().After(deadline) {
				c.finish()
				return nil, fmt.Errorf("i2c: %#x reg %#x: timeout after %d bytes", addr, reg, i)
			}
			time.Sleep(10 * time.Microsecond)
			v = r.Read32(FIFO_RX)
		}
		if length < 0 {
			length = int(v & 0xff)
			continue
		}
		out = append(out, byte(v))
	}
	if err := c.finish(); err != nil {
		return nil, err
	}
	if length < len(out) {
		out = out[:length]
	}
	return out, nil
}

// finish waits for the transfer to end.
func (c *ctl) finish() error {
	deadline := time.Now().Add(Timeout)
	for c.r.Read32(STATUS)&STATUS_XFER_END == 0 {
		if time.Now().After(deadline) {
			return errors.New("i2c: transfer did not end")
		}
		time.Sleep(10 * time.Microsecond)
	}
	return nil
}
