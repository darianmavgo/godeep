package input

import (
	"context"
	"testing"
	"time"
)

func TestDecoder(t *testing.T) {
	var out []byte
	d := Keyboard{Emit: func(b []byte) { out = append(out, b...) }}
	now := time.Unix(0, 0)
	feed := func(mods byte, keys ...byte) { d.State(mods, keys, now) }
	feed(ModLShift, 0x0b) // H
	feed(0)
	feed(0, 0x0c)       // i
	feed(0, 0x0c, 0x1e) // 1 while i is held: only 1 is new
	feed(0)
	feed(ModRShift, 0x1e) // !
	feed(0)
	feed(0, 0x39) // caps lock on
	feed(0)
	feed(0, 0x04)               // A
	feed(ModLShift, 0x04, 0x05) // b (shift inverts caps)
	feed(0)
	feed(0, 0x39) // caps lock off
	feed(0)
	feed(ModLCtrl, 0x06) // ^C
	feed(0)
	feed(0, 0x52)                               // up
	feed(0, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01) // rollover: ignored
	feed(0)
	feed(0, 0x28) // Enter
	feed(0)
	want := "Hi1!Ab\x03\x1b[A\r"
	if string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestRepeat(t *testing.T) {
	var out []byte
	d := Keyboard{Emit: func(b []byte) { out = append(out, b...) }}
	t0 := time.Unix(0, 0)
	d.State(0, []byte{0x04}, t0)
	d.Tick(t0.Add(RepeatDelay - time.Millisecond))
	if string(out) != "a" {
		t.Fatalf("repeated before the delay: %q", out)
	}
	d.Tick(t0.Add(RepeatDelay))
	d.Tick(t0.Add(RepeatDelay + RepeatRate))
	d.Tick(t0.Add(RepeatDelay + RepeatRate + time.Millisecond)) // not due yet
	if string(out) != "aaa" {
		t.Fatalf("repeat: %q", out)
	}
	d.State(0, nil, t0.Add(time.Second))
	d.Tick(t0.Add(2 * time.Second))
	if string(out) != "aaa" {
		t.Fatalf("repeated after release: %q", out)
	}
}

func TestKeyEvents(t *testing.T) {
	var b Broadcaster
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ev := b.Subscribe(ctx)
	d := Keyboard{Events: &b}
	now := time.Unix(0, 0)
	d.State(ModLShift, []byte{0x04}, now)
	d.State(0, []byte{0x04, 0x05}, now)
	d.State(0, nil, now)
	var got []string
	for len(got) < 4 {
		got = append(got, (<-ev).String())
	}
	want := []string{
		"key-down a (usage 0x04, mods 0x02)",
		"key-down b (usage 0x05, mods 0x00)",
		"key-up a (usage 0x04, mods 0x00)",
		"key-up b (usage 0x05, mods 0x00)",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d: %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBroadcaster(t *testing.T) {
	b := Broadcaster{Kind: "mouse"}
	ctx, cancel := context.WithCancel(context.Background())
	slow := b.Subscribe(ctx)
	for i := 0; i < 100; i++ {
		b.Send(Event{Kind: Motion, DX: int32(i)})
	}
	if sent, dropped, subs := b.Stats(); sent != 100 || dropped != 36 || subs != 1 {
		t.Fatalf("sent %d dropped %d subscribers %d", sent, dropped, subs)
	}
	if e := <-slow; e.DX != 0 || e.Time.IsZero() {
		t.Fatalf("first event %+v", e)
	}
	cancel()
	for range slow { // closed after cancel
	}
	b.Send(Event{Kind: Motion})
	if _, _, subs := b.Stats(); subs != 0 {
		t.Fatal("subscriber not removed")
	}
}
