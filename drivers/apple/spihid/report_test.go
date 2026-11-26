package spihid

import (
	"encoding/binary"
	"testing"

	"godeep/kernel/input"
)

// kbdPacket builds a keyboard input packet the way the device sends it.
func kbdPacket(mods byte, keys ...byte) []byte {
	b := make([]byte, PacketSize)
	b[0], b[1] = FlagRead, DevKbd
	binary.LittleEndian.PutUint16(b[6:], dataOffset+reportLen+2)
	msg := b[dataOffset:]
	msg[0], msg[1] = MsgReport, DevKbd
	binary.LittleEndian.PutUint16(msg[6:], reportLen)
	r := msg[dataOffset:]
	r[0], r[1] = reportID, mods
	copy(r[3:9], keys)
	binary.LittleEndian.PutUint16(b[PacketSize-2:], CRC16(b[:PacketSize-2]))
	return b
}

func TestCRC16(t *testing.T) {
	// CRC-16/ARC check value
	if got := CRC16([]byte("123456789")); got != 0xbb3d {
		t.Fatalf("CRC16 = %#x, want 0xbb3d", got)
	}
}

func TestParse(t *testing.T) {
	p, ok := ParsePacket(kbdPacket(input.ModLShift, 0x0b))
	if !ok || !p.CRCOK {
		t.Fatal("packet rejected or CRC mismatch")
	}
	r, ok := p.KeyboardReport()
	if !ok || r[1] != input.ModLShift || r[3] != 0x0b {
		t.Fatalf("report %v %v", r, ok)
	}

	tp := kbdPacket(0, 0x04)
	tp[1] = DevTP
	if p, _ := ParsePacket(tp); func() bool { _, ok := p.KeyboardReport(); return ok }() {
		t.Fatal("trackpad packet taken as keyboard")
	}
	if _, ok := ParsePacket(make([]byte, 10)); ok {
		t.Fatal("short packet accepted")
	}
	bad := kbdPacket(0, 0x04)
	bad[PacketSize-1] ^= 1
	if p, _ := ParsePacket(bad); p.CRCOK {
		t.Fatal("bad CRC accepted")
	}
	if !Empty(make([]byte, PacketSize)) || Empty(kbdPacket(0)) {
		t.Fatal("Empty")
	}
}
