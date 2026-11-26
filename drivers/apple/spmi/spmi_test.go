package spmi

import (
	"testing"

	"godeep/kernel/mmio"
)

// pmic answers extended reads from a byte map.
func pmic(mem map[uint16]byte) *mmio.Fake {
	f := mmio.NewFake()
	var fifo []uint32
	f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		if off == CMD && v&0xf8 == cmdExtReadL {
			n := int(v&7) + 1
			addr := uint16(v >> 16)
			fifo = append(fifo, uint32(1<<n-1)<<16|replyAck|v&0x7f00)
			for i := 0; i < n; i += 4 {
				var w uint32
				for j := 0; j < 4 && i+j < n; j++ {
					w |= uint32(mem[addr+uint16(i+j)]) << (8 * j)
				}
				fifo = append(fifo, w)
			}
		}
		return false
	}
	f.OnRead = func(f *mmio.Fake, off uint64) (uint32, bool) {
		switch off {
		case STATUS:
			if len(fifo) == 0 {
				return RX_EMPTY, true
			}
			return 0, true
		case RSP:
			v := fifo[0]
			fifo = fifo[1:]
			return v, true
		}
		return 0, false
	}
	return f
}

func TestRead(t *testing.T) {
	b := New(pmic(map[uint16]byte{0x1411: 1, 0x1412: 2, 0x1413: 3, 0x1414: 4, 0x1415: 5, 0x1416: 6}))
	got, err := b.Read(0xf, 0x1411, 6)
	if err != nil || len(got) != 6 || got[0] != 1 || got[5] != 6 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := New(mmio.NewFake()).Read(0xf, 0, 1); err == nil {
		t.Fatal("no reply accepted")
	}
}
