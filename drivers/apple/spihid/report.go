package spihid

import (
	"encoding/binary"
	"time"
)

// Packet framing (Asahi spi-hid-apple-core.c, U-Boot apple_spi_kbd.c).
// Every SPI read returns one 256-byte packet:
//
//	flags u8 | device u8 | offset le16 | remain le16 | length le16 | data [246] | crc16 le16
//
// data starts with an 8-byte message header:
//
//	type u8 | device u8 | unknown u8 | id u8 | rsplen le16 | length le16 | payload...
//
// A keyboard input report is flags 0x20 (read), device 1, message type 0x10,
// payload length 10, payload:
//
//	report id (1) | modifiers | reserved | 6 HID usage codes | fn
const (
	PacketSize = 256

	FlagRead   = 0x20
	FlagWrite  = 0x40
	DevMngt    = 0x0
	DevKbd     = 0x1
	DevTP      = 0x2
	MsgReport  = 0x10
	reportLen  = 10
	reportID   = 1
	dataOffset = 8
)

// Packet is a decoded transfer packet.
type Packet struct {
	Flags, Device          byte
	Offset, Remain, Length uint16
	Data                   []byte // 246 bytes
	CRCOK                  bool
}

// ParsePacket decodes a 256-byte packet. CRCOK compares the trailing CRC
// with CRC-16/ARC (Linux crc16()) over the first 254 bytes; U-Boot doesn't
// check it, so neither does the keyboard path: it's only a diagnostic.
func ParsePacket(b []byte) (Packet, bool) {
	if len(b) != PacketSize {
		return Packet{}, false
	}
	le := binary.LittleEndian
	return Packet{
		Flags: b[0], Device: b[1],
		Offset: le.Uint16(b[2:]), Remain: le.Uint16(b[4:]), Length: le.Uint16(b[6:]),
		Data:  b[dataOffset : PacketSize-2],
		CRCOK: CRC16(b[:PacketSize-2]) == le.Uint16(b[PacketSize-2:]),
	}, true
}

// Empty reports whether the packet carries nothing (what a read returns
// when the device has nothing to send): all zeros or all ones.
func Empty(b []byte) bool {
	for _, x := range b[1:] {
		if x != b[0] {
			return false
		}
	}
	return b[0] == 0 || b[0] == 0xff
}

// KeyboardReport returns the 10-byte keyboard report in p, if it is one.
func (p Packet) KeyboardReport() ([]byte, bool) {
	if p.Flags != FlagRead || p.Device != DevKbd || len(p.Data) < dataOffset+reportLen {
		return nil, false
	}
	msg := p.Data
	if msg[0] != MsgReport || binary.LittleEndian.Uint16(msg[6:]) != reportLen {
		return nil, false
	}
	r := msg[dataOffset : dataOffset+reportLen]
	if r[0] != reportID {
		return nil, false
	}
	return r, true
}

// CRC16 is CRC-16/ARC: polynomial 0x8005 reflected (0xa001), initial 0.
func CRC16(b []byte) uint16 {
	var crc uint16
	for _, x := range b {
		crc ^= uint16(x)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xa001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// Key repeat, like a typical console.
const (
	RepeatDelay = 500 * time.Millisecond
	RepeatRate  = 33 * time.Millisecond
)

// Decoder turns successive keyboard reports into console bytes: newly
// pressed keys produce their bytes (US layout), the last one held repeats.
type Decoder struct {
	Emit func([]byte)

	down     [6]byte
	caps     bool
	held     byte // usage code repeating, 0 if none
	heldMods byte
	next     time.Time
}

// Report feeds one 10-byte keyboard report.
func (d *Decoder) Report(r []byte, now time.Time) {
	mods := r[1]
	var cur [6]byte
	copy(cur[:], r[3:9])
	if cur[0] == 0x01 { // ErrorRollOver: too many keys; keep the old state
		return
	}
	pressed := byte(0)
	for _, k := range cur {
		if k == 0 || contains(d.down, k) {
			continue
		}
		if k == 0x39 { // Caps Lock
			d.caps = !d.caps
			continue
		}
		if b := Translate(k, mods, d.caps); len(b) > 0 {
			d.Emit(b)
			pressed = k
		}
	}
	switch {
	case pressed != 0:
		d.held, d.heldMods, d.next = pressed, mods, now.Add(RepeatDelay)
	case !contains(cur, d.held):
		d.held = 0
	default:
		d.heldMods = mods
	}
	d.down = cur
}

// Tick emits repeats of the held key that are due by now.
func (d *Decoder) Tick(now time.Time) {
	if d.held == 0 || now.Before(d.next) {
		return
	}
	if b := Translate(d.held, d.heldMods, d.caps); len(b) > 0 {
		d.Emit(b)
	}
	d.next = now.Add(RepeatRate)
}

func contains(a [6]byte, k byte) bool {
	for _, x := range a {
		if x == k && k != 0 {
			return true
		}
	}
	return false
}

// Modifier bits.
const (
	ModLCtrl  = 1 << 0
	ModLShift = 1 << 1
	ModLAlt   = 1 << 2
	ModLGUI   = 1 << 3
	ModRCtrl  = 1 << 4
	ModRShift = 1 << 5
	ModRAlt   = 1 << 6
	ModRGUI   = 1 << 7
)

// US layout, HID usages 0x1e..0x38: unshifted and shifted.
const (
	plain   = "1234567890\r\x1b\x7f\t -=[]\\#;'`,./"
	shifted = "!@#$%^&*()\r\x1b\x7f\t _+{}|~:\"~<>?"
)

// Translate maps a HID usage code and modifiers to console bytes. Ctrl+key
// gives the control character (Ctrl-C = 0x03, which kills the foreground
// program); arrows give ANSI sequences. Unmapped keys give nothing.
func Translate(k, mods byte, caps bool) []byte {
	shift := mods&(ModLShift|ModRShift) != 0
	ctrl := mods&(ModLCtrl|ModRCtrl) != 0
	switch {
	case k >= 0x04 && k <= 0x1d: // a..z
		c := 'a' + k - 0x04
		if ctrl {
			return []byte{c & 0x1f}
		}
		if shift != caps {
			c -= 'a' - 'A'
		}
		return []byte{c}
	case k >= 0x1e && k <= 0x38:
		t := plain
		if shift {
			t = shifted
		}
		c := t[k-0x1e]
		if k == 0x32 { // non-US # (not on this keyboard)
			return nil
		}
		return []byte{c}
	case k >= 0x4f && k <= 0x52: // right, left, down, up
		return []byte{0x1b, '[', "CDBA"[k-0x4f]}
	case k == 0x4c: // forward delete
		return []byte{0x1b, '[', '3', '~'}
	}
	return nil
}
