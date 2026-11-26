package i2c

import (
	"context"
	"testing"

	"godeep/kernel/mmio"
)

// fakeDevice answers register reads with a length byte and data.
func fakeController(regs map[byte][]byte) *mmio.Fake {
	f := mmio.NewFake()
	var rx []uint32
	var tx []uint32
	f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		switch off {
		case FIFO_TX:
			tx = append(tx, v)
			if v&TX_READ != 0 && len(tx) >= 4 {
				data := regs[byte(tx[1])]
				rx = append(rx, uint32(len(data)))
				for _, b := range data {
					rx = append(rx, uint32(b))
				}
				for len(rx) < int(v&0xff) { // the master clocks every byte it asked for
					rx = append(rx, 0xff)
				}
				tx = nil
				f.Set(STATUS, STATUS_XFER_END)
			}
			return true
		case CONTROL:
			if v&(CONTROL_CLEAR_RX) != 0 {
				rx = nil
			}
		}
		return false
	}
	f.OnRead = func(f *mmio.Fake, off uint64) (uint32, bool) {
		if off != FIFO_RX {
			return 0, false
		}
		if len(rx) == 0 {
			return RX_EMPTY, true
		}
		v := rx[0]
		rx = rx[1:]
		return v, true
	}
	return f
}

func TestReadReg(t *testing.T) {
	f := fakeController(map[byte][]byte{0x1a: {0x41, 0, 0, 0}})
	f.Set(CONTROL, 0x84b) // m1n1's clock divider + enable: kept
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &Bus{reqs: make(chan *req)}
	c := &ctl{r: f, control: f.Get(CONTROL)}
	go func() {
		for q := range b.reqs {
			v, err := c.read(q.addr, q.reg, q.n)
			q.done <- result{v, err}
		}
	}()
	got, err := b.ReadReg(ctx, 0x38, 0x1a, 4)
	if err != nil || len(got) != 4 || got[0] != 0x41 {
		t.Fatalf("got %x, %v", got, err)
	}
	for _, w := range mmio.Writes(f.Log()) {
		if w.Off == CONTROL && w.Val&0xfff != 0x84b|CONTROL_CLEAR_RX|CONTROL_CLEAR_TX {
			t.Fatalf("CONTROL written as %#x", w.Val)
		}
	}
	if _, err := b.ReadReg(ctx, 0x38, 0x99, 4); err != nil {
		t.Fatalf("empty register: %v", err)
	}
}
