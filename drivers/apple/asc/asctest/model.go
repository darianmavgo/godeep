// Package asctest models an Apple ASC mailbox for host tests: the
// firmware side is a function that answers each message the AP sends.
package asctest

import (
	"sync"

	"godeep/drivers/apple/asc"
	"godeep/kernel/mmio"
	"godeep/kernel/rtkit"
)

// Model is a fake ASC mailbox whose firmware side is a function: it sees
// each message the AP sends and may answer with any number of messages.
type Model struct {
	F   *mmio.Fake
	mu  sync.Mutex
	in  []rtkit.Message // to the AP
	cur rtkit.Message
	lo  uint32
}

func NewModel(fw func(rtkit.Message) []rtkit.Message) *Model {
	m := &Model{F: mmio.NewFake()}
	m.F.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		switch off {
		case asc.A2I_SEND0:
			m.lo = v
		case asc.A2I_SEND0 + 4:
			m.cur.Data = uint64(m.lo) | uint64(v)<<32
		case asc.A2I_SEND1 + 4:
			m.cur.EP = uint8(m.lo)
			out := fw(m.cur)
			m.mu.Lock()
			m.in = append(m.in, out...)
			m.mu.Unlock()
		case asc.A2I_SEND1:
			m.lo = v
		}
		return false
	}
	var head rtkit.Message
	m.F.OnRead = func(f *mmio.Fake, off uint64) (uint32, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch off {
		case asc.I2A_CONTROL:
			if len(m.in) == 0 {
				return asc.CONTROL_EMPTY, true
			}
			return 0, true
		case asc.I2A_RECV0:
			head = m.in[0]
			return uint32(head.Data), true
		case asc.I2A_RECV0 + 4:
			return uint32(head.Data >> 32), true
		case asc.I2A_RECV1:
			m.in = m.in[1:] // reading the endpoint word pops the message
			return uint32(head.EP), true
		}
		return 0, false
	}
	return m
}

// Post queues a message from the firmware (e.g. its HELLO after start).
func (m *Model) Post(msgs ...rtkit.Message) {
	m.mu.Lock()
	m.in = append(m.in, msgs...)
	m.mu.Unlock()
}
