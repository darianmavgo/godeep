package input

import (
	"fmt"
	"time"
)

// Key repeat, like a typical console.
const (
	RepeatDelay = 500 * time.Millisecond
	RepeatRate  = 33 * time.Millisecond
)

// Keyboard turns successive HID keyboard states (modifier byte + up to 6
// pressed usages, the boot-keyboard report every HID keyboard can give)
// into console bytes and key events: newly pressed keys produce their bytes
// (US layout), the last one held repeats. Not safe for concurrent use: the
// driver goroutine that owns the device calls it.
type Keyboard struct {
	Emit   func([]byte) // console bytes
	Events *Broadcaster // key events (optional)

	down     [6]byte
	caps     bool
	held     byte // usage code repeating, 0 if none
	heldMods byte
	next     time.Time
}

// State feeds the current modifiers and pressed keys (zeros ignored).
func (d *Keyboard) State(mods byte, keys []byte, now time.Time) {
	var cur [6]byte
	copy(cur[:], keys)
	if cur[0] == 0x01 { // ErrorRollOver: too many keys; keep the old state
		return
	}
	if d.Events != nil {
		for _, k := range d.down {
			if k != 0 && !contains(cur, k) {
				d.Events.Send(Event{Kind: KeyUp, Code: uint16(k), Mods: mods, Time: now})
			}
		}
	}
	pressed := byte(0)
	for _, k := range cur {
		if k == 0 || contains(d.down, k) {
			continue
		}
		if d.Events != nil {
			d.Events.Send(Event{Kind: KeyDown, Code: uint16(k), Mods: mods, Time: now})
		}
		if k == 0x39 { // Caps Lock
			d.caps = !d.caps
			continue
		}
		if b := Translate(k, mods, d.caps); len(b) > 0 {
			if d.Emit != nil {
				d.Emit(b)
			}
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
func (d *Keyboard) Tick(now time.Time) {
	if d.held == 0 || now.Before(d.next) {
		return
	}
	if b := Translate(d.held, d.heldMods, d.caps); len(b) > 0 && d.Emit != nil {
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

// KeyName names a HID keyboard usage for event listings.
func KeyName(k byte) string {
	switch {
	case k >= 0x04 && k <= 0x1d:
		return string(rune('a' + k - 0x04))
	case k >= 0x1e && k <= 0x27:
		return string("1234567890"[k-0x1e])
	}
	if n, ok := keyNames[k]; ok {
		return n
	}
	return fmt.Sprintf("key%#02x", k)
}

var keyNames = map[byte]string{
	0x28: "enter", 0x29: "esc", 0x2a: "backspace", 0x2b: "tab", 0x2c: "space", 0x2d: "-", 0x2e: "=",
	0x2f: "[", 0x30: "]", 0x31: "\\", 0x33: ";", 0x34: "'", 0x35: "`", 0x36: ",", 0x37: ".", 0x38: "/",
	0x39: "capslock", 0x4c: "delete", 0x4f: "right", 0x50: "left", 0x51: "down", 0x52: "up",
	0x3a: "f1", 0x3b: "f2", 0x3c: "f3", 0x3d: "f4", 0x3e: "f5", 0x3f: "f6", 0x40: "f7", 0x41: "f8",
	0x42: "f9", 0x43: "f10", 0x44: "f11", 0x45: "f12",
}
