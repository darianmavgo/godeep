// Package usb holds what every USB host controller driver shares: standard
// descriptors and requests, and the HID boot protocol (keyboards and mice
// in their BIOS-compatible report format, which every HID keyboard and
// mouse must support). Host controllers: drivers/usb/xhci.
package usb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"godeep/kernel/input"
)

// Speed of a device.
type Speed int

const (
	Full  Speed = 1 // 12 Mb/s
	Low   Speed = 2 // 1.5 Mb/s
	High  Speed = 3 // 480 Mb/s
	Super Speed = 4 // 5 Gb/s
)

func (s Speed) String() string {
	switch s {
	case Full:
		return "full speed"
	case Low:
		return "low speed"
	case High:
		return "high speed"
	case Super:
		return "SuperSpeed"
	}
	return fmt.Sprintf("speed %d", int(s))
}

// DefaultMaxPacket0 is the control endpoint's packet size to start with,
// before the device descriptor says.
func (s Speed) DefaultMaxPacket0() int {
	switch s {
	case Low, Full:
		return 8
	case High:
		return 64
	}
	return 512
}

// Standard requests (bRequest) and descriptor types.
const (
	ReqGetDescriptor    = 0x06
	ReqSetConfiguration = 0x09

	DescDevice    = 1
	DescConfig    = 2
	DescInterface = 4
	DescEndpoint  = 5

	// HID class requests
	ReqSetIdle     = 0x0a
	ReqSetProtocol = 0x0b

	ClassHID       = 3
	SubclassBoot   = 1
	ProtoKeyboard  = 1
	ProtoMouse     = 2
	ReqTypeIn      = 0x80 // device to host, standard, device
	ReqTypeClassIf = 0x21 // host to device, class, interface
)

// Setup is a control request.
type Setup struct {
	RequestType, Request byte
	Value, Index, Length uint16
}

// GetDescriptor reads a descriptor of type typ (index idx).
func GetDescriptor(typ, idx byte, n int) Setup {
	return Setup{RequestType: ReqTypeIn, Request: ReqGetDescriptor, Value: uint16(typ)<<8 | uint16(idx), Length: uint16(n)}
}

// DeviceDesc is the device descriptor (the fields drivers need).
type DeviceDesc struct {
	USB             uint16
	Class           byte
	MaxPacket0      int
	Vendor, Product uint16
	Configs         int
}

// ParseDevice decodes an 18-byte device descriptor (or its first 8 bytes,
// enough for MaxPacket0).
func ParseDevice(b []byte) (DeviceDesc, error) {
	if len(b) < 8 || b[1] != DescDevice {
		return DeviceDesc{}, errors.New("usb: not a device descriptor")
	}
	d := DeviceDesc{USB: binary.LittleEndian.Uint16(b[2:]), Class: b[4], MaxPacket0: int(b[7])}
	if d.USB >= 0x0300 {
		d.MaxPacket0 = 1 << b[7] // SuperSpeed: an exponent
	}
	if len(b) >= 18 {
		d.Vendor, d.Product = binary.LittleEndian.Uint16(b[8:]), binary.LittleEndian.Uint16(b[10:])
		d.Configs = int(b[17])
	}
	return d, nil
}

// Endpoint is an endpoint descriptor.
type Endpoint struct {
	Address   byte // bit 7: IN
	Attr      byte // bits 1:0: 0 control, 1 isochronous, 2 bulk, 3 interrupt
	MaxPacket int
	Interval  byte
}

// IsInterruptIn reports an interrupt IN endpoint.
func (e Endpoint) IsInterruptIn() bool { return e.Address&0x80 != 0 && e.Attr&3 == 3 }

// IsBulkIn and IsBulkOut: bulk endpoints (mass storage).
func (e Endpoint) IsBulkIn() bool  { return e.Address&0x80 != 0 && e.Attr&3 == 2 }
func (e Endpoint) IsBulkOut() bool { return e.Address&0x80 == 0 && e.Attr&3 == 2 }

// Interface classes.
const (
	ClassComm        = 0x02 // CDC communications (subclass 6: Ethernet, ECM)
	ClassMassStorage = 0x08
	ClassHub         = 0x09
	ClassCDCData     = 0x0a
	SubclassECM      = 0x06
	SubclassNCM      = 0x0d
	ClassVendor      = 0xff
	DescString       = 0x03
	DescCSInterface  = 0x24 // class-specific interface descriptor (CDC functional)
)

// Num is the endpoint number.
func (e Endpoint) Num() int { return int(e.Address & 0xf) }

// Interface is an interface descriptor with its endpoints.
type Interface struct {
	Number, Alt               byte
	Class, Subclass, Protocol byte
	Endpoints                 []Endpoint
	Extra                     [][]byte // class-specific descriptors (CDC functional ones)
}

// Config is a configuration descriptor with its interfaces.
type Config struct {
	Value      byte
	Interfaces []Interface
}

// ParseConfig decodes a full configuration descriptor (wTotalLength bytes).
func ParseConfig(b []byte) (Config, error) {
	if len(b) < 9 || b[1] != DescConfig {
		return Config{}, errors.New("usb: not a configuration descriptor")
	}
	c := Config{Value: b[5]}
	for i := int(b[0]); i+2 <= len(b); {
		n := int(b[i])
		if n < 2 || i+n > len(b) {
			return c, errors.New("usb: truncated configuration descriptor")
		}
		d := b[i : i+n]
		switch d[1] {
		case DescInterface:
			if n >= 9 {
				c.Interfaces = append(c.Interfaces, Interface{Number: d[2], Alt: d[3], Class: d[5], Subclass: d[6], Protocol: d[7]})
			}
		case DescCSInterface:
			if len(c.Interfaces) > 0 {
				it := &c.Interfaces[len(c.Interfaces)-1]
				it.Extra = append(it.Extra, append([]byte(nil), d...))
			}
		case DescEndpoint:
			if n >= 7 && len(c.Interfaces) > 0 {
				it := &c.Interfaces[len(c.Interfaces)-1]
				it.Endpoints = append(it.Endpoints, Endpoint{Address: d[2], Attr: d[3],
					MaxPacket: int(binary.LittleEndian.Uint16(d[4:]) & 0x7ff), Interval: d[6]})
			}
		}
		i += n
	}
	return c, nil
}

// BootKind says whether an interface is a HID boot keyboard or mouse.
func (i Interface) BootKind() string {
	if i.Class != ClassHID || i.Subclass != SubclassBoot {
		return ""
	}
	switch i.Protocol {
	case ProtoKeyboard:
		return "keyboard"
	case ProtoMouse:
		return "mouse"
	}
	return ""
}

// BootKeyboard decodes boot keyboard reports (modifiers, reserved, 6
// usages) into console bytes and key events.
type BootKeyboard struct{ input.Keyboard }

// Report handles one report.
func (k *BootKeyboard) Report(r []byte, now time.Time) {
	if len(r) >= 8 {
		k.State(r[0], r[2:8], now)
	}
}

// BootMouse decodes boot mouse reports (buttons, dx, dy, optional wheel).
type BootMouse struct {
	Events  *input.Broadcaster
	buttons byte
}

// Report handles one report.
func (m *BootMouse) Report(r []byte, now time.Time) {
	if len(r) < 3 || m.Events == nil {
		return
	}
	for b := 0; b < 3; b++ {
		if (r[0]^m.buttons)&(1<<b) != 0 {
			m.Events.Send(input.Event{Kind: input.Button, Code: uint16(b), Down: r[0]&(1<<b) != 0, Time: now})
		}
	}
	m.buttons = r[0]
	if dx, dy := int32(int8(r[1])), int32(int8(r[2])); dx != 0 || dy != 0 {
		m.Events.Send(input.Event{Kind: input.Motion, DX: dx, DY: dy, Time: now})
	}
	if len(r) >= 4 && r[3] != 0 {
		m.Events.Send(input.Event{Kind: input.Wheel, DY: int32(int8(r[3])), Time: now})
	}
}

// ECMMACIndex returns the string index of a CDC Ethernet interface's MAC
// address (the Ethernet Networking Functional Descriptor's iMACAddress).
func (i Interface) ECMMACIndex() (byte, bool) {
	for _, d := range i.Extra {
		if len(d) >= 4 && d[2] == 0x0f {
			return d[3], true
		}
	}
	return 0, false
}

// UnionSubordinate returns the first subordinate interface of a CDC
// communications interface's Union functional descriptor (its data
// interface).
func (i Interface) UnionSubordinate() (byte, bool) {
	for _, d := range i.Extra {
		if len(d) >= 5 && d[2] == 0x06 {
			return d[4], true
		}
	}
	return 0, false
}

// ParseMAC decodes a MAC address string descriptor: 12 hex digits in UTF-16.
func ParseMAC(desc []byte) ([6]byte, error) {
	var mac [6]byte
	if len(desc) < 2+24 || desc[1] != DescString {
		return mac, errors.New("usb: not a MAC address string")
	}
	hex := func(c byte) (byte, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	for i := 0; i < 6; i++ {
		hi, ok1 := hex(desc[2+4*i])
		lo, ok2 := hex(desc[2+4*i+2])
		if !ok1 || !ok2 {
			return mac, errors.New("usb: bad MAC address string")
		}
		mac[i] = hi<<4 | lo
	}
	return mac, nil
}
