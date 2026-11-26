package hostnet

import (
	"context"
	"testing"
	"time"
)

func TestHostAddr(t *testing.T) {
	for in, want := range map[string]string{":22": "127.0.0.1:2222", "0.0.0.0:8080": "127.0.0.1:8080", ":0": "127.0.0.1:0"} {
		if got, err := HostAddr(in); err != nil || got != want {
			t.Errorf("HostAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestListenClosesWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l, err := Net{}.Listen(ctx, "tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Net{}.Dial(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	done := make(chan struct{})
	go func() { l.Accept(); l.Accept(); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Accept still blocked")
	}
}
