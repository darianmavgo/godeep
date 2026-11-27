package main

import (
	"encoding/binary"
	"testing"

	"godeep/kernel/fdt"
)

func testConfig() *config {
	return &config{out: "unused", kernelAddr: dramBase + 0x8000_0000, width: 3456, height: 2234, format: "x2r10g10b10", e2h: true}
}

// The tree must give board/applem1 what it looks for: a framebuffer
// (ScanEarly and the full parser agree), godeep's region inside /memory and
// clear of /reserved-memory, and the PL011 mirror.
func TestDeviceTree(t *testing.T) {
	c := testConfig()
	c.fault = 3
	blob, err := deviceTree(c)
	if err != nil {
		t.Fatal(err)
	}
	e := fdt.ScanEarly(blob)
	if !e.HasFB || e.FB.Base != fbBase || e.FB.Width != 3456 || e.FB.Stride != 3456*4 || string(e.FB.Format) != "x2r10g10b10" {
		t.Fatalf("early framebuffer = %+v", e)
	}
	if e.UART != uartBase || string(e.Bootargs) != "godeep.testfault=3" {
		t.Fatalf("uart %#x bootargs %q", e.UART, e.Bootargs)
	}

	root, err := fdt.Parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	// board/applem1 mem.go: RAM at DRAM+4 GB (4 GB), DMA right above (512 MB)
	lo, hi := uint64(dramBase+4<<30), uint64(dramBase+8<<30+512<<20)
	mem := root.Find("/memory@10000000000").Reg()
	if len(mem) != 1 || lo < mem[0].Base || hi > mem[0].Base+mem[0].Size {
		t.Fatalf("/memory %+v doesn't contain godeep's region", mem)
	}
	for _, n := range root.Find("/reserved-memory").Children {
		for _, r := range n.Reg() {
			if r.Base < hi && r.Base+r.Size > lo {
				t.Fatalf("reserved %s overlaps godeep's region", n.Name)
			}
		}
	}
	if fb := root.Find("/chosen/framebuffer@103e0000000"); fb == nil || fb.Reg()[0].Size != c.fbSize() {
		t.Fatal("framebuffer node missing or wrong size")
	}
}

func TestPatchShim(t *testing.T) {
	b, err := patchShim(uartBase, hcrRW|hcrE2H, fdtAddr, 0x10080000000)
	if err != nil {
		t.Fatal(err)
	}
	p := b[len(b)-40:]
	for i, want := range []uint64{uartBase, hcrRW | hcrE2H, fdtAddr, 0x10080000000, spinAddr} {
		if got := binary.LittleEndian.Uint64(p[i*8:]); got != want {
			t.Errorf("parameter %d = %#x, want %#x", i, got, want)
		}
	}
	if binary.LittleEndian.Uint64(shimCode[len(shimCode)-40:]) != uartBase {
		t.Error("patchShim modified shimCode in place")
	}
}

func TestPrepareRejects(t *testing.T) {
	for _, mod := range []func(*config){
		func(c *config) { c.kernelAddr += 0x1000 },           // not 2 MB aligned
		func(c *config) { c.kernelAddr = shimAddr },          // on the shim
		func(c *config) { c.kernelAddr = fbBase },            // on the framebuffer
		func(c *config) { c.width, c.height = 16384, 16384 }, // framebuffer too big
	} {
		c := testConfig()
		c.out = t.TempDir()
		mod(c)
		if _, err := prepare(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}
