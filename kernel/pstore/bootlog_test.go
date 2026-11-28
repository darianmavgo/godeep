package pstore

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestBootLog(t *testing.T) {
	half := make([]byte, BootLogSize)
	if _, _, ok := BootLogText(half); ok {
		t.Fatal("zeroed memory reads as a log")
	}
	if lo, hi := BootLogAppend(half, "x"); lo != 0 || hi != 0 {
		t.Fatal("append without a log changed memory")
	}
	BootLogReset(half)
	if binary.LittleEndian.Uint64(half) != BootLogMagic64 {
		t.Fatalf("BootLogMagic64 doesn't match the magic: %x", binary.LittleEndian.Uint64(half))
	}
	// the entry code appends single bytes by hand, as assembly does
	for _, c := range []byte("ERM") {
		n := binary.LittleEndian.Uint32(half[8:])
		half[16+n] = c
		binary.LittleEndian.PutUint32(half[8:], n+1)
	}
	lo, hi := BootLogAppend(half, "\nstage 1\n")
	if lo != 8 || hi != 16+3+9 {
		t.Fatalf("changed [%d,%d)", lo, hi)
	}
	text, trunc, ok := BootLogText(half)
	if !ok || trunc || string(text) != "ERM\nstage 1\n" {
		t.Fatalf("text %q trunc %v ok %v", text, trunc, ok)
	}
}

func TestBootLogFull(t *testing.T) {
	half := make([]byte, BootLogSize+64) // the next half follows: must stay untouched
	for i := BootLogSize; i < len(half); i++ {
		half[i] = 0xaa
	}
	BootLogReset(half)
	line := strings.Repeat("y", 100)
	for i := 0; i < 60; i++ {
		if _, hi := BootLogAppend(half, line); hi > BootLogSize {
			t.Fatalf("wrote up to %d", hi)
		}
	}
	text, trunc, ok := BootLogText(half)
	if !ok || !trunc || len(text) != BootLogSize-16 {
		t.Fatalf("len %d trunc %v ok %v", len(text), trunc, ok)
	}
	for i := BootLogSize; i < len(half); i++ {
		if half[i] != 0xaa {
			t.Fatal("wrote past the half")
		}
	}
}
