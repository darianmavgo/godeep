package pstore

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestRing(t *testing.T) {
	mem := make([]byte, MinSize)
	if r := Open(mem[:MinSize-1]); r != nil {
		t.Fatal("accepted a tiny region")
	}
	r := Open(mem)
	if _, _, ok := r.Previous(); ok {
		t.Fatal("record in zeroed memory")
	}
	r.Write([]byte("hello "))
	r.WriteByte('x')

	// reset without a clean halt
	r2 := Open(mem)
	if got := string(r2.Crash()); got != "hello x" {
		t.Fatalf("crash %q", got)
	}
	if r2.Boots() != 2 {
		t.Fatalf("boots %d", r2.Boots())
	}
	// this boot wraps the ring: the last 256 bytes survive, in order
	var all strings.Builder
	for i := 0; i < 100; i++ {
		line := strings.Repeat(string(rune('a'+i%26)), 9) + "\n"
		all.WriteString(line)
		r2.Write([]byte(line))
	}
	r2.MarkClean()
	r3 := Open(mem)
	out, clean, ok := r3.Previous()
	want := all.String()[all.Len()-256:]
	if !ok || !clean || string(out) != want {
		t.Fatalf("wrapped: ok %v clean %v\n%q\nwant\n%q", ok, clean, out, want)
	}
	if r3.Crash() != nil {
		t.Fatal("a clean halt reported as a crash")
	}
	// a region of another size is not trusted
	if _, _, ok := Open(append(mem, 0)).Previous(); ok {
		t.Fatal("size mismatch accepted")
	}
	var nilRing *Ring
	nilRing.Write([]byte("x"))
	nilRing.MarkClean()
}

func TestConcurrentWrites(t *testing.T) {
	mem := make([]byte, headerSize+1<<17) // no wrap: writers never share a byte
	r := Open(mem)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				r.Write([]byte("0123456789"))
			}
		}()
	}
	wg.Wait()
	out, _, _ := Open(mem).Previous()
	if len(out) != 80000 || bytes.Count(out, []byte("0123456789")) != 8000 {
		t.Fatalf("len %d, whole chunks %d", len(out), bytes.Count(out, []byte("0123456789")))
	}
}
