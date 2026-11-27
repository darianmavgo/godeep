package ipc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPipeCopiesAndEOF(t *testing.T) {
	r, w := Pipe()
	var want bytes.Buffer
	go func() {
		buf := make([]byte, 7)
		for i := 0; i < 1000; i++ {
			copy(buf, []byte("line   "))
			buf[5] = byte('0' + i%10)
			w.Write(buf) // the pipe copies: reusing buf is fine
		}
		w.Close()
	}()
	for i := 0; i < 1000; i++ {
		want.WriteString("line " + string(rune('0'+i%10)) + " ")
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("got %d bytes, want %d", len(got), want.Len())
	}
}

func TestPipeReaderCloseWakesWriter(t *testing.T) {
	r, w := Pipe()
	done := make(chan error)
	go func() {
		for {
			if _, err := w.Write([]byte("y\n")); err != nil {
				done <- err
				return
			}
		}
	}()
	buf := make([]byte, 4)
	io.ReadFull(r, buf)
	r.Close()
	select {
	case err := <-done:
		if !IsClosedPipe(err) {
			t.Fatalf("writer got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer still blocked after reader closed")
	}
}

func TestPipeWriterCloseWakesReader(t *testing.T) {
	r, w := Pipe()
	done := make(chan error)
	go func() {
		_, err := r.Read(make([]byte, 8))
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	w.CloseWithError(errors.New("boom"))
	if err := <-done; err == nil || err.Error() != "boom" {
		t.Fatalf("reader got %v", err)
	}
	w.Close() // idempotent
	r.Close()
	if _, err := w.Write([]byte("x")); !IsClosedPipe(err) {
		t.Fatalf("write after close: %v", err)
	}
}

func TestPipeConcurrentWriters(t *testing.T) {
	r, w := Pipe()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				w.Write([]byte("abc\n"))
			}
		}()
	}
	go func() { wg.Wait(); w.Close() }()
	got, _ := io.ReadAll(r)
	if n := strings.Count(string(got), "abc\n"); n != 800 {
		t.Fatalf("%d lines", n)
	}
}

func TestPortCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns := NewNamespace()
	p := NewPort[string, int]("len")
	unpub, err := ns.Publish(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		go p.Serve(ctx, func(_ context.Context, s string) (int, error) { return len(s), nil })
	}
	q, err := Lookup[string, int](ns, "len")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := q.Call(ctx, "hello"); n != 5 || err != nil {
				t.Errorf("Call = %d, %v", n, err)
			}
		}()
	}
	wg.Wait()
	if p.Calls() != 50 {
		t.Fatalf("calls %d", p.Calls())
	}
	if _, err := Lookup[int, int](ns, "len"); err == nil {
		t.Fatal("wrong types found")
	}
	if _, err := ns.Publish(NewPort[int, int]("len")); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate publish: %v", err)
	}
	unpub()
	if _, err := Lookup[string, int](ns, "len"); err == nil {
		t.Fatal("found after unpublish")
	}
}

func TestPortCallCancelled(t *testing.T) {
	p := NewPort[int, int]("none")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Call(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no server: %v", err)
	}
	// A server that blocks: the caller still returns on its own ctx.
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	go p.Serve(sctx, func(ctx context.Context, _ int) (int, error) { <-ctx.Done(); return 0, ctx.Err() })
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if _, err := p.Call(ctx2, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow server: %v", err)
	}
}

func TestNamespaceView(t *testing.T) {
	ns := NewNamespace()
	ns.Publish(NewPort[int, int]("a"))
	ns.Publish(NewPort[int, int]("b"))
	v := ns.Only("a")
	if l := v.List(); len(l) != 1 || l[0].Name() != "a" {
		t.Fatalf("view lists %v", l)
	}
	if _, err := Lookup[int, int](v, "b"); !errors.Is(err, ErrPermission) {
		t.Fatalf("view found b: %v", err)
	}
	if _, err := v.Publish(NewPort[int, int]("c")); !errors.Is(err, ErrPermission) {
		t.Fatalf("view published c: %v", err)
	}
	if _, err := v.Only("a", "b").Publish(NewPort[int, int]("b2")); err == nil {
		t.Fatal("nested view widened")
	}
	if _, err := Lookup[int, int](v.Only("b"), "a"); err == nil {
		t.Fatal("nested view: a visible")
	}
}
