package sh

import (
	"context"
	"strings"
	"testing"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/input"
	"godeep/kernel/proc"
	"godeep/kernel/tty"
)

func TestEvtest(t *testing.T) {
	mouse := &input.Broadcaster{Kind: "mouse"}
	dev.Register(dev.Driver{Compatible: "test,mouse", Run: func(ctx context.Context, d *dev.Device) error {
		d.Ready(mouse)
		<-ctx.Done()
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := dev.NewManager(proc.NewTable(), nil, nil)
	m.Start(ctx, []dev.Node{{Name: "usbmouse0", Compatible: "test,mouse"}})
	if _, err := m.Get(ctx, "usbmouse0"); err != nil {
		t.Fatal(err)
	}
	out := &syncBuf{}
	term := tty.New(make(chan byte), out, false)
	defer term.Close()
	done := make(chan error, 1)
	go func() { done <- evtest(ctx, Env{TTY: term, Dev: m}, nil, []string{"0.3"}) }()
	for !strings.Contains(out.String(), "listening") {
		time.Sleep(time.Millisecond)
	}
	mouse.Send(input.Event{Kind: input.Motion, DX: 3, DY: -1})
	mouse.Send(input.Event{Kind: input.Button, Code: 1, Down: true})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"input: usbmouse0/mouse", "usbmouse0/mouse: motion dx=3 dy=-1", "usbmouse0/mouse: button 1 down"} {
		if !strings.Contains(s, want) {
			t.Fatalf("output lacks %q:\n%s", want, s)
		}
	}
}
