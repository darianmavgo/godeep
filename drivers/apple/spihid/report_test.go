package spihid

import (
	"encoding/binary"
	"testing"
	"time"
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
	p, ok := ParsePacket(kbdPacket(ModLShift, 0x0b))
	if !ok || !p.CRCOK {
		t.Fatal("packet rejected or CRC mismatch")
	}
	r, ok := p.KeyboardReport()
	if !ok || r[1] != ModLShift || r[3] != 0x0b {
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

func TestDecoder(t *testing.T) {
	var out []byte
	d := Decoder{Emit: func(b []byte) { out = append(out, b...) }}
	now := time.Unix(0, 0)
	feed := func(mods byte, keys ...byte) {
		r := make([]byte, reportLen)
		r[0], r[1] = reportID, mods
		copy(r[3:], keys)
		d.Report(r, now)
	}
	feed(ModLShift, 0x0b) // H
	feed(0)
	feed(0, 0x0c)       // i
	feed(0, 0x0c, 0x1e) // 1 while i is held: only 1 is new
	feed(0)
	feed(ModRShift, 0x1e) // !
	feed(0)
	feed(0, 0x39) // caps lock on
	feed(0)
	feed(0, 0x04)               // A
	feed(ModLShift, 0x04, 0x05) // b (shift inverts caps)
	feed(0)
	feed(0, 0x39) // caps lock off
	feed(0)
	feed(ModLCtrl, 0x06) // ^C
	feed(0)
	feed(0, 0x52)                               // up
	feed(0, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01) // rollover: ignored
	feed(0)
	feed(0, 0x28) // Enter
	feed(0)
	want := "Hi1!Ab\x03\x1b[A\r"
	if string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestRepeat(t *testing.T) {
	var out []byte
	d := Decoder{Emit: func(b []byte) { out = append(out, b...) }}
	t0 := time.Unix(0, 0)
	r := []byte{reportID, 0, 0, 0x04, 0, 0, 0, 0, 0, 0}
	d.Report(r, t0)
	d.Tick(t0.Add(RepeatDelay - time.Millisecond))
	if string(out) != "a" {
		t.Fatalf("repeated before the delay: %q", out)
	}
	d.Tick(t0.Add(RepeatDelay))
	d.Tick(t0.Add(RepeatDelay + RepeatRate))
	d.Tick(t0.Add(RepeatDelay + RepeatRate + time.Millisecond)) // not due yet
	if string(out) != "aaa" {
		t.Fatalf("repeat: %q", out)
	}
	d.Report([]byte{reportID, 0, 0, 0, 0, 0, 0, 0, 0, 0}, t0.Add(time.Second))
	d.Tick(t0.Add(2 * time.Second))
	if string(out) != "aaa" {
		t.Fatalf("repeated after release: %q", out)
	}
}
