package klog

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

func TestLog(t *testing.T) {
	con := &syncBuf{}
	l := New(con, 200)
	l.Write([]byte("part"))
	if len(l.Bytes()) != 0 || con.String() != "" {
		t.Fatal("partial line logged")
	}
	l.Write([]byte("ial\nsecond\n"))
	if b := string(l.Bytes()); !strings.Contains(b, "] partial\n") || !strings.Contains(b, "] second\n") {
		t.Fatalf("log %q", b)
	}
	if con.String() != string(l.Bytes()) {
		t.Fatal("console mirror differs")
	}
	for i := 0; i < 50; i++ {
		l.Printf("line %d", i)
	}
	b := string(l.Bytes())
	if len(b) > 200 || !strings.HasPrefix(b, "[") || !strings.Contains(b, "line 49\n") || l.Dropped() == 0 {
		t.Fatalf("ring: %d bytes, dropped %d: %q", len(b), l.Dropped(), b)
	}
}

func TestFollow(t *testing.T) {
	l := New(nil, 0)
	ctx, cancel := context.WithCancel(context.Background())
	c := l.Follow(ctx)
	l.Printf("hello")
	select {
	case line := <-c:
		if !strings.HasSuffix(string(line), "] hello\n") {
			t.Fatalf("%q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing followed")
	}
	cancel()
	for range c { // closed after cancel
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l.Printf("x") }()
	}
	wg.Wait()
}
