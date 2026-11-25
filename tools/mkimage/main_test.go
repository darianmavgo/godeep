package main

import (
	"encoding/binary"
	"os"
	"testing"
)

// Builds an Image from the real kernel ELF when it has been built.
func TestBuildFromKernel(t *testing.T) {
	const elfPath = "../../bin/godeep-arm64.elf"
	if _, err := os.Stat(elfPath); err != nil {
		t.Skip("run `make kernel` first")
	}
	img, _, err := build(elfPath, 0x40000000)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	if le.Uint32(img[56:]) != 0x644d5241 {
		t.Fatal("bad magic")
	}
	if code0 := le.Uint32(img[0:]); code0>>26 != 0x05 { // B imm26
		t.Fatalf("code0 %#x is not a branch", code0)
	}
	if off := le.Uint64(img[8:]); off == 0 || off >= 2<<20 {
		t.Fatalf("text_offset %#x", off)
	}
	if size := le.Uint64(img[16:]); size != uint64(len(img)) {
		t.Fatalf("image_size %d != %d", size, len(img))
	}
}
