// Package input is godeep's input layer: the events keyboards, mice and
// trackpads produce, a Source drivers publish them through, and the HID
// keyboard decoder every keyboard shares (Apple's SPI keyboard, USB boot
// keyboards) to turn key state into console bytes.
//
// There is no global event device: consumers find Sources among the device
// manager's services (like disks), and the console gets typed bytes on its
// input channel, which the board hands to keyboard drivers.
package input

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Kind is an event type.
type Kind uint8

const (
	KeyDown Kind = iota + 1 // Code: HID usage (keyboard page)
	KeyUp
	Motion // relative pointer motion: DX, DY
	Button // Code: button number (0 left, 1 right, 2 middle), Down
	Touch  // absolute contacts: X, Y (device units), Code: contact slot, Down
	Wheel  // DY: wheel clicks (positive = away from the user)
)

func (k Kind) String() string {
	switch k {
	case KeyDown:
		return "key-down"
	case KeyUp:
		return "key-up"
	case Motion:
		return "motion"
	case Button:
		return "button"
	case Touch:
		return "touch"
	case Wheel:
		return "wheel"
	}
	return fmt.Sprintf("kind(%d)", k)
}

// Event is one input event.
type Event struct {
	Kind   Kind
	Code   uint16
	Down   bool
	Mods   byte // keyboard modifiers at the time (HID bits, see Mod*)
	DX, DY int32
	X, Y   int32
	Time   time.Time
}

func (e Event) String() string {
	switch e.Kind {
	case KeyDown, KeyUp:
		return fmt.Sprintf("%s %s (usage %#02x, mods %#02x)", e.Kind, KeyName(byte(e.Code)), e.Code, e.Mods)
	case Motion, Wheel:
		return fmt.Sprintf("%s dx=%d dy=%d", e.Kind, e.DX, e.DY)
	case Button:
		return fmt.Sprintf("button %d %s", e.Code, upDown(e.Down))
	case Touch:
		return fmt.Sprintf("touch %d %s x=%d y=%d", e.Code, upDown(e.Down), e.X, e.Y)
	}
	return e.Kind.String()
}

func upDown(d bool) string {
	if d {
		return "down"
	}
	return "up"
}

// Source is the service an input driver publishes.
type Source interface {
	// Subscribe returns events from now until ctx is done. A subscriber
	// that falls behind loses events (counted), never blocks the driver.
	Subscribe(ctx context.Context) <-chan Event
	// Kind says what the device is: "keyboard", "mouse", "trackpad"...
	InputKind() string
}

// Sources is the service of a driver with several input devices (Apple's
// SPI keyboard + trackpad).
type Sources interface{ InputSources() []Source }

// All returns the sources a service provides (none if it isn't one).
func All(svc any) []Source {
	switch s := svc.(type) {
	case Sources:
		return s.InputSources()
	case Source:
		return []Source{s}
	}
	return nil
}

// Broadcaster fans a driver's events out to subscribers. The zero value
// is ready; embed it and set Kind.
type Broadcaster struct {
	Kind string

	mu      sync.Mutex
	subs    map[chan Event]struct{}
	sent    int
	dropped int
}

// InputKind implements Source.
func (b *Broadcaster) InputKind() string { return b.Kind }

// Subscribe implements Source.
func (b *Broadcaster) Subscribe(ctx context.Context) <-chan Event {
	c := make(chan Event, 64)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[chan Event]struct{}{}
	}
	b.subs[c] = struct{}{}
	b.mu.Unlock()
	context.AfterFunc(ctx, func() {
		b.mu.Lock()
		delete(b.subs, c)
		b.mu.Unlock()
		close(c)
	})
	return c
}

// Send delivers e to every subscriber without blocking.
func (b *Broadcaster) Send(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent++
	for c := range b.subs {
		select {
		case c <- e:
		default:
			b.dropped++
		}
	}
}

// Stats reports events sent, lost by slow subscribers, and subscribers.
func (b *Broadcaster) Stats() (sent, dropped, subscribers int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent, b.dropped, len(b.subs)
}
