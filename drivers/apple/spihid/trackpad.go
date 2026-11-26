package spihid

import (
	"encoding/binary"
	"time"

	"godeep/kernel/input"
)

// Trackpad reports (Asahi hid-magicmouse.c, the SPI path). Every report
// starts with a 6-byte mouse header:
//
//	report id 0x02 | buttons | rel x (s8) | rel y (s8) | 4 bytes
//
// In multitouch mode (switched on with a set-report, mtEnable) a 38-byte
// touch header and 30-byte contacts follow:
//
//	header: 22 bytes | contacts u8 | buttons u8 | 14 bytes
//	contact (le16 each): ?, ?, abs x, abs y, rel x, rel y, tool major, tool
//	  minor, orientation, touch major, touch minor, 2 unused bytes,
//	  pressure, multi
//
// abs y grows away from the user. A contact with touch major 0 isn't
// touching. Report id 0x60 means the touch controller reset itself and
// multitouch mode must be switched on again.
const (
	tpReportID    = 0x02
	tpResetID     = 0x60
	tpMouseHdr    = 6
	tpTouchHdr    = 38
	tpContact     = 30
	tpCountAt     = tpMouseHdr + 22
	tpButtonsAt   = tpMouseHdr + 23
	tpContactsAt  = tpMouseHdr + tpTouchHdr
	tpMaxContacts = 16
)

// mtEnable is the set-report payload that switches multitouch mode on
// (report 0x02, value 0x01: hid-magicmouse's feature_mt_trackpad2_usb,
// which it also uses for the 2021 MacBook Pro SPI trackpads).
var mtEnable = []byte{0x02, 0x01}

// Pointer tuning. Trackpad units: the 14" pad is about 13000 wide.
const (
	MotionDivisor = 8   // trackpad units per pointer unit
	WheelStep     = 400 // two-finger travel per wheel click
)

// Trackpad turns reports into pointer events: one finger moves the
// pointer, two fingers scroll, a click is button 0. Every contact is also
// reported as a Touch event. Not safe for concurrent use.
type Trackpad struct {
	Events *input.Broadcaster

	buttons    byte
	contacts   map[int][2]int32 // slot → (x, y) of the last report
	accX, accY int32
	wheel      int32
	touchMode  bool // multitouch reports seen
}

// Report handles one input report. reset: the device asks for multitouch
// mode again.
func (t *Trackpad) Report(p []byte, now time.Time) (ok, reset bool) {
	if len(p) >= 1 && p[0] == tpResetID {
		t.contacts = nil
		return true, true
	}
	if len(p) < tpMouseHdr || p[0] != tpReportID {
		return false, false
	}
	buttons := p[1]
	var cur map[int][2]int32
	if len(p) >= tpContactsAt && (len(p)-tpContactsAt)%tpContact == 0 {
		n := int(p[tpCountAt])
		if n > tpMaxContacts || tpContactsAt+n*tpContact > len(p) {
			return false, false
		}
		t.touchMode = true
		buttons |= p[tpButtonsAt]
		cur = map[int][2]int32{}
		le := binary.LittleEndian
		for i := 0; i < n; i++ {
			c := p[tpContactsAt+i*tpContact:]
			if le.Uint16(c[18:]) == 0 { // touch major: not touching
				continue
			}
			cur[i] = [2]int32{int32(int16(le.Uint16(c[4:]))), int32(int16(le.Uint16(c[6:])))}
		}
	}

	send := func(e input.Event) {
		if t.Events != nil {
			e.Time = now
			t.Events.Send(e)
		}
	}
	if (buttons^t.buttons)&1 != 0 {
		send(input.Event{Kind: input.Button, Code: 0, Down: buttons&1 != 0})
	}
	t.buttons = buttons

	if cur == nil { // plain mouse report
		if dx, dy := int32(int8(p[2])), int32(int8(p[3])); dx != 0 || dy != 0 {
			send(input.Event{Kind: input.Motion, DX: dx, DY: dy})
		}
		return true, false
	}

	for slot, xy := range cur {
		send(input.Event{Kind: input.Touch, Code: uint16(slot), Down: true, X: xy[0], Y: xy[1]})
	}
	for slot, xy := range t.contacts {
		if _, ok := cur[slot]; !ok {
			send(input.Event{Kind: input.Touch, Code: uint16(slot), Down: false, X: xy[0], Y: xy[1]})
		}
	}
	// Movement of contacts present in both reports (screen y grows down).
	var dx, dy, moved int32
	for slot, xy := range cur {
		if old, ok := t.contacts[slot]; ok {
			dx += xy[0] - old[0]
			dy -= xy[1] - old[1]
			moved++
		}
	}
	switch {
	case len(cur) == 1 && moved == 1:
		t.accX += dx
		t.accY += dy
		mx, my := t.accX/MotionDivisor, t.accY/MotionDivisor
		t.accX -= mx * MotionDivisor
		t.accY -= my * MotionDivisor
		if mx != 0 || my != 0 {
			send(input.Event{Kind: input.Motion, DX: mx, DY: my})
		}
	case len(cur) == 2 && moved == 2:
		t.wheel -= dy / 2 // fingers moving away from the user: positive
		for t.wheel >= WheelStep || t.wheel <= -WheelStep {
			step := int32(1)
			if t.wheel < 0 {
				step = -1
			}
			t.wheel -= step * WheelStep
			send(input.Event{Kind: input.Wheel, DY: step})
		}
	default:
		t.accX, t.accY, t.wheel = 0, 0, 0
	}
	t.contacts = cur
	return true, false
}
