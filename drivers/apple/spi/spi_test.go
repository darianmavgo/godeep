package spi

import (
	"bytes"
	"context"
	"testing"
	"time"

	"godeep/kernel/mmio"
)

// model is an SPI controller with 16-entry FIFOs and a device on the bus
// that answers with resp. Every FIFOSTAT or IF_XFER read advances the bus
// by one byte.
type model struct {
	f            *mmio.Fake
	running      bool
	txcnt, rxcnt int
	txq, rxq     []byte
	sent         []byte
	resp         []byte
	maxRX        int
}

func newModel(resp []byte) *model {
	m := &model{f: mmio.NewFake(), resp: resp}
	m.f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		switch off {
		case CTRL:
			if v&(CTRL_TX_RESET|CTRL_RX_RESET) != 0 {
				m.txq, m.rxq, m.running = nil, nil, false
				f.Set(IF_XFER, 0)
			}
			m.running = v&CTRL_RUN != 0
		case TXCNT:
			m.txcnt = int(v)
		case RXCNT:
			m.rxcnt = int(v)
		case TXDATA:
			if len(m.txq) >= FifoDepth {
				panic("TX FIFO overflow")
			}
			m.txq = append(m.txq, byte(v))
		case IF_XFER:
			f.Set(IF_XFER, f.Get(IF_XFER)&^v) // W1C
			return true
		}
		return false
	}
	m.f.OnRead = func(f *mmio.Fake, off uint64) (uint32, bool) {
		switch off {
		case FIFOSTAT:
			m.step()
			return uint32(len(m.txq))<<8 | uint32(len(m.rxq))<<24, true
		case IF_XFER:
			m.step() // the bus runs on while software waits
			return 0, false
		case RXDATA:
			if len(m.rxq) == 0 {
				panic("RX FIFO underflow")
			}
			b := m.rxq[0]
			m.rxq = m.rxq[1:]
			return uint32(b), true
		}
		return 0, false
	}
	return m
}

func (m *model) step() {
	if !m.running {
		return
	}
	if m.txcnt > 0 && len(m.txq) > 0 {
		m.sent = append(m.sent, m.txq[0])
		m.txq = m.txq[1:]
		m.txcnt--
		if m.txcnt == 0 {
			m.f.Set(IF_XFER, m.f.Get(IF_XFER)|XFER_TXCOMPLETE)
		}
	}
	if m.rxcnt > 0 && len(m.rxq) < FifoDepth {
		m.rxq = append(m.rxq, m.resp[0])
		m.resp = m.resp[1:]
		m.rxcnt--
	}
	m.maxRX = max(m.maxRX, len(m.rxq))
}

func TestRead(t *testing.T) {
	want := make([]byte, 256)
	for i := range want {
		want[i] = byte(i * 7)
	}
	m := newModel(append([]byte{}, want...))
	c := Controller{m.f}
	c.Init()
	if got := c.SetSpeed(24_000_000, 8_000_000); got != 8_000_000 || m.f.Get(CLKDIV) != 3 {
		t.Fatalf("speed %d, CLKDIV %d", got, m.f.Get(CLKDIV))
	}
	if m.f.Get(PIN) != PIN_CS {
		t.Fatal("CS not inactive after Init")
	}
	c.CS(true)
	got := make([]byte, 256)
	if err := c.Transfer(context.Background(), nil, got); err != nil {
		t.Fatal(err)
	}
	c.CS(false)
	if !bytes.Equal(got, want) {
		t.Fatal("received data differs")
	}
	if m.maxRX > FifoDepth {
		t.Fatal("RX FIFO overfilled")
	}
}

func TestWrite(t *testing.T) {
	m := newModel(nil)
	c := Controller{m.f}
	c.Init()
	tx := bytes.Repeat([]byte{0x40, 0x01, 0xaa}, 30)
	if err := c.Transfer(context.Background(), tx, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.sent, tx) {
		t.Fatalf("sent %d bytes, want %d", len(m.sent), len(tx))
	}
}

func TestTimeout(t *testing.T) {
	f := mmio.NewFake() // a controller that never moves data
	if err := (Controller{f}).Transfer(context.Background(), nil, make([]byte, 4)); err == nil {
		t.Fatal("no timeout")
	}
	if f.Get(CTRL) != 0 {
		t.Fatal("controller left running after a timeout")
	}
}

func TestClockDivClamp(t *testing.T) {
	f := mmio.NewFake()
	(Controller{f}).SetSpeed(24_000_000, 100_000_000)
	if f.Get(CLKDIV) != CLKDIV_MIN {
		t.Fatalf("CLKDIV %d", f.Get(CLKDIV))
	}
	(Controller{f}).SetSpeed(24_000_000, 1)
	if f.Get(CLKDIV) != CLKDIV_MAX {
		t.Fatalf("CLKDIV %d", f.Get(CLKDIV))
	}
}

// A send-only transfer must finish (or time out) even if the controller
// reports receive data: that once spun the only core forever.
func TestSendWithRXLevel(t *testing.T) {
	f := mmio.NewFake()
	f.Set(FIFOSTAT, 16<<24) // RX full, TX empty, forever
	f.Set(IF_XFER, XFER_TXCOMPLETE)
	f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool { return off == IF_XFER } // W1C: ignore
	done := make(chan error, 1)
	go func() { done <- (Controller{f}).Transfer(context.Background(), make([]byte, 256), nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send-only transfer never returned")
	}
}
