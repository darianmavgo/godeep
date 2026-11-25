package kernel

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Boot the whole system, run ps and echo through the shell, then close the console.
func TestBootShellSession(t *testing.T) {
	in := make(chan byte, 256)
	for _, b := range []byte("echo hi there\nworkers 3 &\nps\nps -g\n") {
		in <- b
	}
	out := &syncBuf{}
	done := make(chan error)
	go func() { done <- Boot(context.Background(), Console{In: in, Out: out}) }()
	// Close input once the last command has printed, which makes init shut down.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "coreutils.workers") {
		if time.Now().After(deadline) {
			t.Fatalf("timed out; output:\n%s", out)
		}
		select {
		case err := <-done:
			t.Fatalf("boot ended early: %v\n%s", err, out)
		default:
		}
		runtimeYield()
	}
	close(in)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"godeep " + Version, "hi there", "] workers", "GOROUTINES", "sh", "workers", "running"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in output:\n%s", want, s)
		}
	}
}
