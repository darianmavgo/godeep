package edit

import "unicode/utf8"

// Decoder turns terminal input bytes into Keys: UTF-8 text, control keys,
// and the ESC [ / ESC O sequences terminals send for arrows, Home, End,
// PgUp, PgDn and Delete. A lone ESC is reported when the next byte isn't
// '[' or 'O' (or by Flush, when input pauses).
type Decoder struct {
	buf []byte
}

// Feed adds one byte and returns the keys it completes.
func (d *Decoder) Feed(b byte) []Key {
	d.buf = append(d.buf, b)
	var keys []Key
	for len(d.buf) > 0 {
		k, n := decode(d.buf)
		if n == 0 {
			break // incomplete
		}
		d.buf = d.buf[n:]
		if k != (Key{}) {
			keys = append(keys, k)
		}
	}
	return keys
}

// Flush reports a pending lone ESC (the caller saw input pause).
func (d *Decoder) Flush() []Key {
	if len(d.buf) == 1 && d.buf[0] == 0x1b {
		d.buf = d.buf[:0]
		return []Key{{Code: Escape}}
	}
	return nil
}

// decode reads one key from b: the key and the bytes used (0: need more).
func decode(b []byte) (Key, int) {
	c := b[0]
	switch c {
	case 0x1b:
		if len(b) < 2 {
			return Key{}, 0
		}
		if b[1] != '[' && b[1] != 'O' {
			return Key{Code: Escape}, 1
		}
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				return csiKey(string(b[2:i]), b[i]), i + 1
			}
			if i > 16 {
				return Key{}, i + 1 // garbage: drop it
			}
		}
		return Key{}, 0
	case '\r', '\n':
		return Key{Code: Enter}, 1
	case '\t':
		return Key{Code: Tab}, 1
	case 0x7f, 0x08:
		return Key{Code: Backspace}, 1
	case 0x01:
		return Key{Code: CtrlA}, 1
	case 0x03:
		return Key{Code: CtrlC}, 1
	case 0x05:
		return Key{Code: CtrlE}, 1
	case 0x06:
		return Key{Code: CtrlF}, 1
	case 0x07:
		return Key{Code: CtrlG}, 1
	case 0x0b:
		return Key{Code: CtrlK}, 1
	case 0x11:
		return Key{Code: CtrlQ}, 1
	case 0x13:
		return Key{Code: CtrlS}, 1
	case 0x15:
		return Key{Code: CtrlU}, 1
	}
	if c < 0x20 {
		return Key{}, 1 // other control keys: ignored
	}
	if c < 0x80 {
		return Key{Rune: rune(c)}, 1
	}
	if !utf8.FullRune(b) {
		return Key{}, 0
	}
	r, n := utf8.DecodeRune(b)
	if r == utf8.RuneError {
		return Key{}, n
	}
	return Key{Rune: r}, n
}

func csiKey(params string, final byte) Key {
	switch final {
	case 'A':
		return Key{Code: Up}
	case 'B':
		return Key{Code: Down}
	case 'C':
		return Key{Code: Right}
	case 'D':
		return Key{Code: Left}
	case 'H':
		return Key{Code: Home}
	case 'F':
		return Key{Code: End}
	case '~':
		switch params {
		case "1", "7":
			return Key{Code: Home}
		case "4", "8":
			return Key{Code: End}
		case "3":
			return Key{Code: Delete}
		case "5":
			return Key{Code: PageUp}
		case "6":
			return Key{Code: PageDown}
		}
	}
	return Key{}
}
