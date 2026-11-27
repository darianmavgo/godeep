package usb

import (
	"errors"
	"fmt"
	"time"

	"godeep/kernel/input"
)

// HID in report protocol: the device describes its reports with a report
// descriptor, and Report decodes them with it. This covers what boot
// protocol can't: absolute pointers (tablets, touch screens, QEMU's
// usb-tablet), mice with more buttons or a 16-bit range, keyboards that
// only speak report protocol, and devices with several report IDs (a
// keyboard and a mouse in one interface).

// HID class descriptor types and requests.
const (
	DescHID          = 0x21
	DescHIDReport    = 0x22
	ReqTypeInStdIf   = 0x81 // device to host, standard, interface
	SubclassNone     = 0
	usagePageDesktop = 0x01
	usagePageKeys    = 0x07
	usagePageButton  = 0x09
	usageX           = 0x30
	usageY           = 0x31
	usageWheel       = 0x38
)

// HIDReportLength returns the report descriptor's length from an
// interface's HID descriptor (in Extra), 0 if there is none.
func (i Interface) HIDReportLength() int {
	for _, d := range i.Extra {
		if len(d) >= 9 && d[1] == DescHID && d[6] == DescHIDReport {
			return int(d[7]) | int(d[8])<<8
		}
	}
	return 0
}

// Field is one Input item of a report: Count values of Size bits each.
type Field struct {
	ReportID   byte
	Offset     int // bit offset in the report (after the report ID byte)
	Size       int
	Count      int
	Page       uint16
	Usages     []uint16 // one per value (variables), or the array's usage range
	UsageMin   uint16   // arrays: the usage of logical value LogMin
	LogMin     int32
	LogMax     int32
	Array      bool // values are usage indexes (keyboard key arrays)
	Relative   bool
	Constant   bool
	usageCount int
}

// ReportDesc is a parsed report descriptor: its Input fields.
type ReportDesc struct {
	Fields    []Field
	reportIDs bool
	bits      map[byte]int // report size in bits, per ID
}

// HasReportIDs says whether reports start with a report ID byte.
func (r *ReportDesc) HasReportIDs() bool { return r.reportIDs }

// Kind is what the device looks like: "keyboard", "tablet" (an absolute
// pointer), "mouse", or "" when nothing here is decoded.
func (r *ReportDesc) Kind() string {
	var keys, abs, rel bool
	for _, f := range r.Fields {
		switch {
		case f.Page == usagePageKeys:
			keys = true
		case f.Page == usagePageDesktop && f.has(usageX):
			if f.Relative {
				rel = true
			} else {
				abs = true
			}
		}
	}
	switch {
	case keys:
		return "keyboard"
	case abs:
		return "tablet"
	case rel:
		return "mouse"
	}
	return ""
}

func (f *Field) has(u uint16) bool {
	for _, x := range f.Usages {
		if x == u {
			return true
		}
	}
	return false
}

// usage returns the usage of value i of a variable field: its own, the
// last one repeated, or 0.
func (f *Field) usage(i int) uint16 {
	if len(f.Usages) == 0 {
		return 0
	}
	if i < len(f.Usages) {
		return f.Usages[i]
	}
	return f.Usages[len(f.Usages)-1]
}

// ParseReportDesc decodes a HID report descriptor's items (HID 1.11 §6.2.2):
// global state (page, logical range, size, count, ID; Push/Pop), local
// usages, and the Input items that make up reports. Output and Feature
// items only advance their report's size.
func ParseReportDesc(b []byte) (*ReportDesc, error) {
	type global struct {
		page           uint16
		logMin, logMax int32
		size, count    int
		id             byte
	}
	var g global
	var stack []global
	var usages []uint16
	var umin, umax uint32
	haveMin, haveMax := false, false
	r := &ReportDesc{bits: map[byte]int{}}
	outBits := map[byte]int{}
	featBits := map[byte]int{}
	resetLocal := func() { usages, haveMin, haveMax = nil, false, false }
	depth := 0
	for i := 0; i < len(b); {
		p := b[i]
		if p == 0xfe { // long item: skip
			if i+2 >= len(b) {
				return nil, errors.New("hid: truncated long item")
			}
			i += 3 + int(b[i+1])
			continue
		}
		n := int(p & 3)
		if n == 3 {
			n = 4
		}
		if i+1+n > len(b) {
			return nil, errors.New("hid: truncated item")
		}
		var u uint32
		for k := 0; k < n; k++ {
			u |= uint32(b[i+1+k]) << (8 * k)
		}
		var sv int32 // signed value
		switch n {
		case 1:
			sv = int32(int8(u))
		case 2:
			sv = int32(int16(u))
		case 4:
			sv = int32(u)
		}
		i += 1 + n
		tag, typ := p>>4, (p>>2)&3
		switch typ {
		case 0: // main
			switch tag {
			case 0x8: // Input
				f := Field{ReportID: g.id, Offset: r.bits[g.id], Size: g.size, Count: g.count, Page: g.page,
					LogMin: g.logMin, LogMax: g.logMax,
					Constant: u&1 != 0, Array: u&2 == 0, Relative: u&4 != 0}
				r.bits[g.id] += g.size * g.count
				pageOf := func(x uint32) uint16 { // 4-byte usages carry their own page
					if x>>16 != 0 {
						f.Page = uint16(x >> 16)
					}
					return uint16(x)
				}
				if haveMin && haveMax && umax >= umin && umax-umin < 1024 {
					f.UsageMin = pageOf(umin)
					for x := umin; x <= umax; x++ {
						f.Usages = append(f.Usages, uint16(x))
					}
				} else {
					f.Usages = usages
					if len(usages) > 0 {
						f.UsageMin = usages[0]
					}
				}
				if !f.Constant && g.size > 0 && g.size <= 32 && g.count > 0 {
					r.Fields = append(r.Fields, f)
				}
			case 0x9: // Output
				outBits[g.id] += g.size * g.count
			case 0xb: // Feature
				featBits[g.id] += g.size * g.count
			case 0xa: // Collection
				depth++
			case 0xc: // End Collection
				depth--
				if depth < 0 {
					return nil, errors.New("hid: unbalanced End Collection")
				}
			}
			resetLocal()
		case 1: // global
			switch tag {
			case 0x0:
				g.page = uint16(u)
			case 0x1:
				g.logMin = sv
			case 0x2:
				g.logMax = sv
				if g.logMin >= 0 && sv < g.logMin { // an unsigned maximum (e.g. 0xff with 1 byte)
					g.logMax = int32(u)
				}
			case 0x7:
				g.size = int(u)
			case 0x8:
				if u == 0 || u > 255 {
					return nil, fmt.Errorf("hid: report ID %d", u)
				}
				g.id = byte(u)
				r.reportIDs = true
			case 0x9:
				g.count = int(u)
			case 0xa: // Push
				stack = append(stack, g)
			case 0xb: // Pop
				if len(stack) == 0 {
					return nil, errors.New("hid: Pop without Push")
				}
				g = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			}
		case 2: // local
			switch tag {
			case 0x0:
				if n == 4 && u>>16 != 0 {
					g.page = uint16(u >> 16)
				}
				usages = append(usages, uint16(u))
			case 0x1:
				umin, haveMin = u, true
			case 0x2:
				umax, haveMax = u, true
			}
		}
	}
	if len(r.Fields) == 0 {
		return nil, errors.New("hid: no input fields")
	}
	return r, nil
}

// HIDDevice decodes reports from one report-protocol interface into input
// events (and, for keyboards, console bytes through Keyboard).
type HIDDevice struct {
	Desc     *ReportDesc
	Events   *input.Broadcaster // pointer events
	Keyboard *input.Keyboard    // nil unless Desc is a keyboard

	buttons uint32
	lastX   int32
	lastY   int32
	havePos bool
}

// Report handles one report (with its ID byte first if the descriptor uses
// IDs).
func (h *HIDDevice) Report(r []byte, now time.Time) {
	id := byte(0)
	if h.Desc.reportIDs {
		if len(r) == 0 {
			return
		}
		id, r = r[0], r[1:]
	}
	var mods byte
	var keys []byte
	keyboard := false
	buttons := h.buttons
	var dx, dy, wheel int32
	x, y := int32(-1), int32(-1)
	for fi := range h.Desc.Fields {
		f := &h.Desc.Fields[fi]
		if f.ReportID != id {
			continue
		}
		for i := 0; i < f.Count; i++ {
			raw, ok := bits(r, f.Offset+i*f.Size, f.Size)
			if !ok {
				break
			}
			v := int32(raw)
			if f.LogMin < 0 { // sign-extend
				v = int32(raw<<(32-f.Size)) >> (32 - f.Size)
			}
			if f.Array {
				if f.Page == usagePageKeys {
					keyboard = true
					if v >= f.LogMin && v <= f.LogMax {
						if k := int32(f.UsageMin) + v - f.LogMin; k > 0 && k < 0xe0 {
							keys = append(keys, byte(k))
						}
					}
				}
				continue
			}
			u := f.usage(i)
			switch f.Page {
			case usagePageKeys:
				keyboard = true
				if u >= 0xe0 && u <= 0xe7 && v != 0 {
					mods |= 1 << (u - 0xe0)
				}
			case usagePageButton:
				if u >= 1 && u <= 32 {
					if v != 0 {
						buttons |= 1 << (u - 1)
					} else {
						buttons &^= 1 << (u - 1)
					}
				}
			case usagePageDesktop:
				switch {
				case u == usageX && f.Relative:
					dx += v
				case u == usageY && f.Relative:
					dy += v
				case u == usageX:
					x = scale(v, f.LogMin, f.LogMax)
				case u == usageY:
					y = scale(v, f.LogMin, f.LogMax)
				case u == usageWheel:
					wheel += v
				}
			}
		}
	}
	if keyboard && h.Keyboard != nil {
		if len(keys) > 6 {
			keys = keys[:6]
		}
		h.Keyboard.State(mods, keys, now)
	}
	if h.Events == nil {
		return
	}
	if x >= 0 || y >= 0 {
		if x < 0 {
			x = h.lastX
		}
		if y < 0 {
			y = h.lastY
		}
		if !h.havePos || x != h.lastX || y != h.lastY {
			h.Events.Send(input.Event{Kind: input.Absolute, X: x, Y: y, Time: now})
		}
		h.lastX, h.lastY, h.havePos = x, y, true
	}
	if dx != 0 || dy != 0 {
		h.Events.Send(input.Event{Kind: input.Motion, DX: dx, DY: dy, Time: now})
	}
	for b := 0; b < 32; b++ {
		if (buttons^h.buttons)&(1<<b) != 0 {
			h.Events.Send(input.Event{Kind: input.Button, Code: uint16(b), Down: buttons&(1<<b) != 0, Time: now})
		}
	}
	h.buttons = buttons
	if wheel != 0 {
		h.Events.Send(input.Event{Kind: input.Wheel, DY: wheel, Time: now})
	}
}

// scale maps v in [lo, hi] to 0..input.AbsMax.
func scale(v, lo, hi int32) int32 {
	if hi <= lo {
		return 0
	}
	v = min(max(v, lo), hi)
	return int32(int64(v-lo) * input.AbsMax / int64(hi-lo))
}

// bits reads n bits (LSB first) at bit offset off.
func bits(b []byte, off, n int) (uint32, bool) {
	if off+n > 8*len(b) {
		return 0, false
	}
	var v uint32
	for i := 0; i < n; i++ {
		p := off + i
		if b[p/8]&(1<<(p%8)) != 0 {
			v |= 1 << i
		}
	}
	return v, true
}
