package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"testing"
)

func fakeImage(tag string, size int) []byte {
	b := make([]byte, size)
	binary.LittleEndian.PutUint32(b[0x38:], imageMagic)
	copy(b[imageHdrBytes:], tag)
	return b
}

func fakeFDT() []byte {
	b := make([]byte, 64)
	binary.BigEndian.PutUint32(b[0:], 0xd00dfeed)
	binary.BigEndian.PutUint32(b[4:], 64)  // totalsize
	binary.BigEndian.PutUint32(b[20:], 17) // version
	return b
}

func gz(b []byte) []byte {
	var w bytes.Buffer
	zw := gzip.NewWriter(&w)
	zw.Write(b)
	zw.Close()
	return w.Bytes()
}

// bootBin mimics Asahi's: m1n1 | config | dtb | gzip(non-Image) decoy | dtb | gzip(U-Boot) | padding.
func bootBin() (bin []byte, prefixLen int) {
	var b bytes.Buffer
	b.Write([]byte("m1n1 stage2 code\x1f\x8b\x08 not really gzip"))
	b.WriteString("chosen.asahi,efi-system-partition=ABCD\n")
	b.Write(fakeFDT())
	b.Write(gz([]byte("a gzipped payload that is not an Image")))
	b.Write(fakeFDT())
	prefixLen = b.Len()
	b.Write(gz(fakeImage("U-Boot 2026.07", 4096)))
	b.Write(make([]byte, 13)) // trailing zero padding
	return b.Bytes(), prefixLen
}

func TestFindKernel(t *testing.T) {
	bin, pre := bootBin()
	p, err := findKernel(bin)
	if err != nil {
		t.Fatal(err)
	}
	if p.Off != pre {
		t.Fatalf("kernel payload at %d, want %d", p.Off, pre)
	}
	if identify(p.Image) != "U-Boot" {
		t.Fatalf("identify = %q", identify(p.Image))
	}
}

func TestBuildReplacesUBoot(t *testing.T) {
	bin, pre := bootBin()
	k := fakeImage("godeep kernel", 8192)
	out, kept, err := build(bin, k, "")
	if err != nil {
		t.Fatal(err)
	}
	if kept != pre || !bytes.Equal(out[:pre], bin[:pre]) {
		t.Fatal("m1n1 and its other payloads were not kept byte for byte")
	}
	p, err := findKernel(out)
	if err != nil {
		t.Fatal(err)
	}
	if p.Off != pre || !bytes.Equal(p.Image, k) || identify(p.Image) != "godeep" {
		t.Fatalf("output kernel payload: off %d, %s", p.Off, identify(p.Image))
	}
	// Rebuilding from the output (godeep already installed) gives the same result.
	again, _, err := build(out, k, "")
	if err != nil {
		t.Fatal(err)
	}
	// bootargs: a chosen.bootargs line right before the kernel
	withArgs, _, err := build(bin, k, "godeep.nosmp")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := findKernel(withArgs); err != nil || !bytes.HasSuffix(withArgs[:p.Off], []byte("chosen.bootargs=godeep.nosmp\n")) {
		t.Fatalf("bootargs line not before the kernel: %v", err)
	}
	if _, _, err := build(bin, k, "a\nb"); err == nil {
		t.Fatal("multi-line bootargs accepted")
	}
	if err != nil || !bytes.Equal(again, out) {
		t.Fatal("build is not idempotent")
	}
}

func TestRejects(t *testing.T) {
	bin, _ := bootBin()
	if _, _, err := build(bin, []byte("not an image, too short"), ""); err == nil {
		t.Error("accepted a non-Image kernel")
	}
	if _, err := findKernel([]byte("m1n1 with nothing after it")); err == nil {
		t.Error("found a kernel in a payload-less boot.bin")
	}
	trailing := append(append([]byte{}, bin...), 'x')
	if _, err := findKernel(trailing); err == nil {
		t.Error("accepted data after the kernel payload")
	}
	noFDT := append([]byte("m1n1"), gz(fakeImage("U-Boot", 4096))...)
	if _, _, err := build(noFDT, fakeImage("godeep", 4096), ""); err == nil {
		t.Error("accepted a boot.bin with no device tree")
	}
}
