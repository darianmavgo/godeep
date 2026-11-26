package fdt

import (
	"os"
	"testing"
)

// m1n1Like builds a tree shaped like m1n1's: the framebuffer under /chosen
// with 2/2 cells, plus a PL011 at the root.
func m1n1Like(t testing.TB) []byte {
	var b Builder
	b.Begin("")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.PropStrings("model", "Apple MacBook Pro (16-inch, M1 Pro, 2021)")
	b.Begin("chosen")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Prop("ranges", nil)
	b.PropStrings("stdout-path", "/pl011@60000000")
	b.PropStrings("bootargs", "quiet godeep.testfault=3")
	b.Begin("framebuffer@103e0000000")
	b.PropStrings("compatible", "apple,simple-framebuffer", "simple-framebuffer")
	b.PropRegs(2, 2, Range{Base: 0x103_e000_0000, Size: 3456 * 2234 * 4})
	b.PropU32("width", 3456)
	b.PropU32("height", 2234)
	b.PropU32("stride", 3456*4)
	b.PropStrings("format", "x2r10g10b10")
	b.End()
	b.End()
	b.Begin("memory@10000000000")
	b.PropStrings("device_type", "memory")
	b.PropRegs(2, 2, Range{Base: 0x100_0000_0000, Size: 0x3_e000_0000})
	b.End()
	b.Begin("pl011@60000000")
	b.PropStrings("compatible", "arm,pl011", "arm,primecell")
	b.PropRegs(2, 2, Range{Base: 0x6000_0000, Size: 0x1000})
	b.End()
	b.End()
	blob, err := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func TestBuilderRoundTrip(t *testing.T) {
	root, err := Parse(m1n1Like(t))
	if err != nil {
		t.Fatal(err)
	}
	if m := root.Strings("model"); len(m) != 1 || m[0] != "Apple MacBook Pro (16-inch, M1 Pro, 2021)" {
		t.Fatalf("model = %q", m)
	}
	fb := root.Find("/chosen/framebuffer@103e0000000")
	if fb == nil {
		t.Fatal("framebuffer node missing")
	}
	if r := fb.Reg(); len(r) != 1 || r[0].Base != 0x103_e000_0000 || r[0].Size != 3456*2234*4 {
		t.Fatalf("reg = %+v", r)
	}
	if c := fb.Strings("compatible"); len(c) != 2 || c[1] != "simple-framebuffer" {
		t.Fatalf("compatible = %q", c)
	}
}

func TestBuilderErrors(t *testing.T) {
	var b Builder
	b.Begin("")
	if _, err := b.Bytes(); err == nil {
		t.Error("unclosed node accepted")
	}
	var c Builder
	c.End()
	if _, err := c.Bytes(); err == nil {
		t.Error("End without Begin accepted")
	}
}

func TestScanEarlyM1n1(t *testing.T) {
	e := ScanEarly(m1n1Like(t))
	if !e.HasFB {
		t.Fatal("no framebuffer found")
	}
	f := e.FB
	if f.Base != 0x103_e000_0000 || f.Size != 3456*2234*4 || f.Width != 3456 || f.Height != 2234 ||
		f.Stride != 3456*4 || string(f.Format) != "x2r10g10b10" {
		t.Fatalf("fb = %+v %q", e.FB, e.FB.Format)
	}
	if e.UART != 0x6000_0000 {
		t.Fatalf("uart = %#x", e.UART)
	}
	if string(e.Bootargs) != "quiet godeep.testfault=3" {
		t.Fatalf("bootargs = %q", e.Bootargs)
	}
}

func TestScanEarlyQEMUVirt(t *testing.T) {
	blob, err := os.ReadFile("testdata/qemu-virt.dtb")
	if err != nil {
		t.Fatal(err)
	}
	e := ScanEarly(blob)
	if e.HasFB {
		t.Error("QEMU virt has no framebuffer, found one")
	}
	if e.UART != 0x0900_0000 {
		t.Errorf("uart = %#x, want 0x9000000", e.UART)
	}
}

func TestScanEarlyNoAllocs(t *testing.T) {
	blob := m1n1Like(t)
	if n := testing.AllocsPerRun(100, func() { ScanEarly(blob) }); n != 0 {
		t.Fatalf("ScanEarly allocates (%v per run); it must not, it runs before the heap exists", n)
	}
}

// Every truncation and every single-byte corruption must give a result
// without panicking.
func TestScanEarlyGarbage(t *testing.T) {
	blob := m1n1Like(t)
	for i := 0; i < len(blob); i++ {
		ScanEarly(blob[:i])
		c := append([]byte{}, blob...)
		c[i] ^= 0xff
		ScanEarly(c)
	}
}

func FuzzScanEarly(f *testing.F) {
	f.Add(m1n1Like(f))
	f.Fuzz(func(t *testing.T, b []byte) { ScanEarly(b) })
}
