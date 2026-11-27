package simplefb

import (
	"sync"

	"godeep/kernel/input"
)

// WheelLines is how many lines one wheel click scrolls.
const WheelLines = 3

// Mouse makes the console respond to pointing devices, like gpm on a Linux
// text console:
//
//   - motion moves the pointer (shown from the first motion on); an
//     absolute device (a tablet) puts it where it points;
//   - the wheel (two-finger scroll on a trackpad) scrolls through history,
//     and any key returns to the live screen;
//   - left button: press and drag to select text (drawn inverted); the text
//     is copied when the button is released, a plain click clears;
//   - right or middle button: paste the copied text as typed input.
//
// Feed it every input event with HandleInput (keyboards' too, so a key
// press ends scrollback).
type Mouse struct {
	C *Console
	// Paste delivers pasted text to the console's input (nil: no paste).
	Paste func([]byte)
	// Speed multiplies pointer motion (0: the console's text scale, so a
	// mouse covers a high-density screen at a usable speed).
	Speed int

	mu   sync.Mutex
	clip string
}

// HandleInput implements kernel.InputHandler.
func (m *Mouse) HandleInput(e input.Event) {
	switch e.Kind {
	case input.Motion:
		s := m.Speed
		if s <= 0 {
			s = m.C.ptrScale()
		}
		m.C.MovePointer(int(e.DX)*s, int(e.DY)*s)
	case input.Absolute:
		m.C.PointTo(int(e.X), int(e.Y), input.AbsMax)
	case input.Wheel:
		m.C.ScrollView(int(e.DY) * WheelLines)
	case input.KeyDown:
		m.C.ScrollView(-History - m.C.rows)
	case input.Button:
		switch {
		case e.Code == 0 && e.Down:
			m.C.SelectStart()
		case e.Code == 0:
			if t := m.C.SelectEnd(); t != "" {
				m.mu.Lock()
				m.clip = t
				m.mu.Unlock()
			}
		case e.Down && m.Paste != nil:
			m.mu.Lock()
			t := m.clip
			m.mu.Unlock()
			if t != "" {
				m.Paste([]byte(t))
			}
		}
	}
}

// Clip is the last copied selection.
func (m *Mouse) Clip() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clip
}

// PasteTo returns a Paste function that types into a console input channel
// without blocking (text beyond the channel's room is dropped).
func PasteTo(in chan<- byte) func([]byte) {
	return func(p []byte) {
		for _, b := range p {
			select {
			case in <- b:
			default:
				return
			}
		}
	}
}
