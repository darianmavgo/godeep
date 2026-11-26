package sh

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"godeep/kernel/dev"
	"godeep/kernel/proc"
	"godeep/kernel/tty"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fakeReboot struct{ n chan struct{} }

func (f fakeReboot) Reboot()           { close(f.n) }
func (f fakeReboot) Devices() []string { return []string{"armed"} }

func TestLsdevFilterAndReboot(t *testing.T) {
	r := fakeReboot{make(chan struct{})}
	dev.Register(dev.Driver{Compatible: "test,wdt", Run: func(ctx context.Context, d *dev.Device) error {
		d.Ready(r)
		<-ctx.Done()
		return nil
	}})
	dev.Register(dev.Driver{Compatible: "test,other", Run: func(ctx context.Context, d *dev.Device) error {
		d.Ready(dev.Lister(nil))
		<-ctx.Done()
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := dev.NewManager(proc.NewTable(), nil, nil)
	m.Start(ctx, []dev.Node{{Name: "watchdog@2922b0000", Compatible: "test,wdt"}, {Name: "spi@1", Compatible: "test,other"}})
	for _, n := range []string{"watchdog@2922b0000", "spi@1"} {
		if _, err := m.Get(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	out := &syncBuf{}
	term := tty.New(make(chan byte), out, false)
	defer term.Close()
	env := Env{TTY: term, Dev: m}

	if err := lsdev(ctx, env, nil, []string{"watchdog"}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "watchdog@2922b0000") || !strings.Contains(s, "armed") || strings.Contains(s, "spi@1") {
		t.Fatalf("lsdev watchdog:\n%s", s)
	}

	go reboot(ctx, env, nil, nil)
	<-r.n
}

func TestRebootWithoutDevice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := dev.NewManager(proc.NewTable(), nil, nil)
	m.Start(ctx, nil)
	term := tty.New(make(chan byte), &syncBuf{}, false)
	defer term.Close()
	if err := reboot(ctx, Env{TTY: term, Dev: m}, nil, nil); err == nil || !strings.Contains(err.Error(), "no reboot device") {
		t.Fatalf("err = %v", err)
	}
}
