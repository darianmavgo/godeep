package usb

import (
	"context"
	"testing"
	"time"

	"godeep/kernel/input"
)

// QEMU's usb-tablet (hw/usb/dev-hid.c): 3 buttons, absolute 16-bit X/Y
// over 0..0x7fff, a relative wheel.
var tabletDesc = []byte{
	0x05, 0x01, 0x09, 0x02, 0xa1, 0x01, 0x09, 0x01, 0xa1, 0x00,
	0x05, 0x09, 0x19, 0x01, 0x29, 0x03, 0x15, 0x00, 0x25, 0x01, 0x95, 0x03, 0x75, 0x01, 0x81, 0x02,
	0x95, 0x01, 0x75, 0x05, 0x81, 0x01,
	0x05, 0x01, 0x09, 0x30, 0x09, 0x31, 0x15, 0x00, 0x26, 0xff, 0x7f, 0x35, 0x00, 0x46, 0xff, 0x7f,
	0x75, 0x10, 0x95, 0x02, 0x81, 0x02,
	0x05, 0x01, 0x09, 0x38, 0x15, 0x81, 0x25, 0x7f, 0x35, 0x00, 0x45, 0x00, 0x75, 0x08, 0x95, 0x01, 0x81, 0x06,
	0xc0, 0xc0,
}

// HID 1.11 appendix B.1: the boot keyboard's report descriptor.
var keyboardDesc = []byte{
	0x05, 0x01, 0x09, 0x06, 0xa1, 0x01, 0x05, 0x07, 0x19, 0xe0, 0x29, 0xe7, 0x15, 0x00, 0x25, 0x01,
	0x75, 0x01, 0x95, 0x08, 0x81, 0x02, 0x95, 0x01, 0x75, 0x08, 0x81, 0x01, 0x95, 0x05, 0x75, 0x01,
	0x05, 0x08, 0x19, 0x01, 0x29, 0x05, 0x91, 0x02, 0x95, 0x01, 0x75, 0x03, 0x91, 0x01, 0x95, 0x06,
	0x75, 0x08, 0x15, 0x00, 0x25, 0x65, 0x05, 0x07, 0x19, 0x00, 0x29, 0x65, 0x81, 0x00, 0xc0,
}

// A wireless receiver's style: keyboard (ID 1) and a 5-button mouse with
// 12-bit motion (ID 2) on one interface.
var comboDesc = []byte{
	0x05, 0x01, 0x09, 0x06, 0xa1, 0x01, 0x85, 0x01,
	0x05, 0x07, 0x19, 0xe0, 0x29, 0xe7, 0x15, 0x00, 0x25, 0x01, 0x75, 0x01, 0x95, 0x08, 0x81, 0x02,
	0x95, 0x06, 0x75, 0x08, 0x15, 0x00, 0x26, 0xff, 0x00, 0x05, 0x07, 0x19, 0x00, 0x2a, 0xff, 0x00, 0x81, 0x00,
	0xc0,
	0x05, 0x01, 0x09, 0x02, 0xa1, 0x01, 0x85, 0x02, 0x09, 0x01, 0xa1, 0x00,
	0x05, 0x09, 0x19, 0x01, 0x29, 0x05, 0x15, 0x00, 0x25, 0x01, 0x95, 0x05, 0x75, 0x01, 0x81, 0x02,
	0x95, 0x01, 0x75, 0x03, 0x81, 0x03,
	0x05, 0x01, 0x16, 0x01, 0xf8, 0x26, 0xff, 0x07, 0x75, 0x0c, 0x95, 0x02, 0x09, 0x30, 0x09, 0x31, 0x81, 0x06,
	0x15, 0x81, 0x25, 0x7f, 0x75, 0x08, 0x95, 0x01, 0x09, 0x38, 0x81, 0x06,
	0xc0, 0xc0,
}

func collect(t *testing.T, b *input.Broadcaster) (<-chan input.Event, func() []input.Event) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := b.Subscribe(ctx)
	return c, func() []input.Event {
		var out []input.Event
		for {
			select {
			case e := <-c:
				out = append(out, e)
			case <-time.After(20 * time.Millisecond):
				return out
			}
		}
	}
}

func TestTablet(t *testing.T) {
	d, err := ParseReportDesc(tabletDesc)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind() != "tablet" || d.HasReportIDs() {
		t.Fatalf("kind %q ids %v", d.Kind(), d.HasReportIDs())
	}
	ev := &input.Broadcaster{Kind: "tablet"}
	_, drain := collect(t, ev)
	h := &HIDDevice{Desc: d, Events: ev}
	now := time.Now()
	h.Report([]byte{0x01, 0xff, 0x3f, 0xff, 0x7f, 0xff}, now) // left, x=0x3fff, y=0x7fff, wheel -1
	got := drain()
	want := []string{"absolute x=16383 y=32767", "button 0 down", "wheel dx=0 dy=-1"}
	if len(got) != len(want) {
		t.Fatalf("events %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("event %d: %q, want %q", i, got[i], want[i])
		}
	}
	h.Report([]byte{0x00, 0xff, 0x3f, 0xff, 0x7f, 0x00}, now) // same place, button up
	if got := drain(); len(got) != 1 || got[0].String() != "button 0 up" {
		t.Fatalf("release: %v", got)
	}
}

func TestReportKeyboard(t *testing.T) {
	d, err := ParseReportDesc(keyboardDesc)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind() != "keyboard" {
		t.Fatalf("kind %q", d.Kind())
	}
	var typed []byte
	h := &HIDDevice{Desc: d, Keyboard: &input.Keyboard{Emit: func(b []byte) { typed = append(typed, b...) }}}
	now := time.Now()
	h.Report([]byte{0x02, 0, 0x04, 0, 0, 0, 0, 0}, now) // shift + a
	h.Report([]byte{0, 0, 0, 0, 0, 0, 0, 0}, now)
	h.Report([]byte{0, 0, 0x05, 0, 0, 0, 0, 0}, now) // b
	if string(typed) != "Ab" {
		t.Fatalf("typed %q", typed)
	}
}

func TestComboWithReportIDs(t *testing.T) {
	d, err := ParseReportDesc(comboDesc)
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasReportIDs() || d.Kind() != "keyboard" {
		t.Fatalf("ids %v kind %q", d.HasReportIDs(), d.Kind())
	}
	var typed []byte
	ev := &input.Broadcaster{Kind: "mouse"}
	_, drain := collect(t, ev)
	h := &HIDDevice{Desc: d, Events: ev, Keyboard: &input.Keyboard{Emit: func(b []byte) { typed = append(typed, b...) }}}
	now := time.Now()
	h.Report([]byte{1, 0x00, 0x1e, 0, 0, 0, 0, 0}, now) // "1"
	// ID 2: buttons 0b10000 (button 5), dx=-3 (0xffd), dy=+5 (0x005), wheel +1
	// 12-bit fields packed LSB first: dx occupies bits 0-11, dy bits 12-23.
	h.Report([]byte{2, 0x10, 0xfd, 0x5f, 0x00, 0x01}, now)
	if string(typed) != "1" {
		t.Fatalf("typed %q", typed)
	}
	got := drain()
	want := []string{"motion dx=-3 dy=5", "button 4 down", "wheel dx=0 dy=1"}
	if len(got) != len(want) {
		t.Fatalf("events %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("event %d: %q, want %q", i, got[i], want[i])
		}
	}
	h.Report([]byte{9, 1, 2, 3}, now) // unknown ID: ignored
	h.Report(nil, now)
}

func TestReportDescErrors(t *testing.T) {
	for _, b := range [][]byte{
		{0x05},             // truncated
		{0xc0},             // End Collection first
		{0xb4},             // Pop without Push
		{0x85, 0x00},       // report ID 0
		{0x05, 0x01, 0xc0}, // nothing to decode... and unbalanced
	} {
		if _, err := ParseReportDesc(b); err == nil {
			t.Errorf("% x: no error", b)
		}
	}
}

func FuzzReportDesc(f *testing.F) {
	f.Add(tabletDesc, []byte{1, 2, 3, 4, 5, 6})
	f.Add(comboDesc, []byte{2, 0x10, 0xfd, 0x5f, 0, 1})
	f.Add(keyboardDesc, []byte{0, 0, 4, 5, 6, 7, 8, 9})
	f.Fuzz(func(t *testing.T, desc, report []byte) {
		d, err := ParseReportDesc(desc)
		if err != nil {
			return
		}
		h := &HIDDevice{Desc: d, Events: &input.Broadcaster{}, Keyboard: &input.Keyboard{}}
		h.Report(report, time.Now())
		d.Kind()
	})
}
