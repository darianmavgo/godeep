package spihid

import "encoding/binary"

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

	FlagRead     = 0x20
	FlagWrite    = 0x40
	DevMngt      = 0x0
	DevKbd       = 0x1
	DevTP        = 0x2
	MsgReport    = 0x10
	MsgSetReport = 0x52
	reportLen    = 10
	reportID     = 1
	dataOffset   = 8
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

// KeyboardReport returns the 10-byte keyboard report in a single-packet
// message, if it is one.
func (p Packet) KeyboardReport() ([]byte, bool) {
	m, ok := ParseMessage(p.Flags, p.Device, p.Data)
	if !ok {
		return nil, false
	}
	return m.KeyboardReport()
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

// Message is a decoded message: the packet's flags and device, the header
// bytes, and the payload.
type Message struct {
	Flags, Device       byte
	Type, Sub, Sub2, ID byte
	Payload             []byte
}

// ParseMessage decodes a message (8-byte header, payload, CRC) from b.
func ParseMessage(flags, device byte, b []byte) (Message, bool) {
	if len(b) < dataOffset {
		return Message{}, false
	}
	n := int(binary.LittleEndian.Uint16(b[6:]))
	if dataOffset+n > len(b) {
		return Message{}, false
	}
	return Message{Flags: flags, Device: device, Type: b[0], Sub: b[1], Sub2: b[2], ID: b[3],
		Payload: b[dataOffset : dataOffset+n]}, true
}

// KeyboardReport returns the 10-byte keyboard report in m, if it is one.
// (U-Boot's checks: read, keyboard, report, length 10, report id 1.)
func (m Message) KeyboardReport() ([]byte, bool) {
	if m.Flags != FlagRead || m.Device != DevKbd || m.Type != MsgReport || m.Sub != DevKbd ||
		len(m.Payload) != reportLen || m.Payload[0] != reportID {
		return nil, false
	}
	return m.Payload, true
}

// TrackpadReport returns a trackpad input report's payload, if m is one.
func (m Message) TrackpadReport() ([]byte, bool) {
	if m.Flags != FlagRead || m.Device != DevTP || m.Type != MsgReport {
		return nil, false
	}
	return m.Payload, true
}

// Booted is what the device sends once after power-on.
var Booted = []byte{0xa0, 0x80, 0x00, 0x00}

// StatusOK is the 4 bytes the device answers a write with.
var StatusOK = []byte{0xac, 0x27, 0x68, 0xd5}

// IsBooted reports whether p is the device's "booted" packet.
func (p Packet) IsBooted() bool {
	return p.Flags == FlagRead && p.Length == 4 && string(p.Data[:4]) == string(Booted)
}

// maxMessage bounds a reassembled message.
const maxMessage = 0x800

// assembler joins messages split over several packets (offset/remain), one
// in progress per device.
type assembler struct{ buf map[byte][]byte }

func (a *assembler) add(p Packet) ([]byte, bool) {
	if p.Offset == 0 && p.Remain == 0 {
		return p.Data, true
	}
	if a.buf == nil {
		a.buf = map[byte][]byte{}
	}
	n := min(int(p.Length), len(p.Data))
	b := a.buf[p.Device]
	if p.Offset == 0 {
		b = b[:0]
	}
	if int(p.Offset) != len(b) || len(b)+n > maxMessage {
		delete(a.buf, p.Device) // a piece is missing: drop the message
		return nil, false
	}
	b = append(b, p.Data[:n]...)
	if p.Remain != 0 {
		a.buf[p.Device] = b
		return nil, false
	}
	delete(a.buf, p.Device)
	return b, true
}

// WritePacket builds a packet that sends a message to device (Asahi's
// spihid_apple_request): header type/sub/sub2, a message id, the response
// length expected, the payload, the message CRC, then the packet CRC.
func WritePacket(device, typ, sub, sub2, id byte, rsplen uint16, payload []byte) []byte {
	le := binary.LittleEndian
	b := make([]byte, PacketSize)
	b[0], b[1] = FlagWrite, device
	le.PutUint16(b[6:], uint16(dataOffset+len(payload)+2))
	m := b[dataOffset:]
	m[0], m[1], m[2], m[3] = typ, sub, sub2, id
	le.PutUint16(m[4:], rsplen)
	le.PutUint16(m[6:], uint16(len(payload)))
	copy(m[dataOffset:], payload)
	le.PutUint16(m[dataOffset+len(payload):], CRC16(m[:dataOffset+len(payload)]))
	le.PutUint16(b[PacketSize-2:], CRC16(b[:PacketSize-2]))
	return b
}
