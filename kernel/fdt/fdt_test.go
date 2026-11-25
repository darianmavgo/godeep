package fdt

import (
	"os"
	"testing"
)

// testdata/qemu-virt.dtb: qemu-system-aarch64 -machine virt,gic-version=3,dumpdtb=...
// -m 4G -device virtio-rng-device
func load(t *testing.T) *Node {
	t.Helper()
	b, err := os.ReadFile("testdata/qemu-virt.dtb")
	if err != nil {
		t.Fatal(err)
	}
	root, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestQEMUVirt(t *testing.T) {
	root := load(t)
	byCompat := map[string]*Node{}
	root.Walk(func(n *Node) {
		if c := n.Strings("compatible"); len(c) > 0 {
			byCompat[c[0]] = n
		}
	})

	gpio := byCompat["arm,pl061"]
	if gpio == nil || gpio.Path != "/pl061@9030000" {
		t.Fatalf("pl061: %+v", gpio)
	}
	if r := gpio.Reg(); len(r) != 1 || r[0].Base != 0x09030000 || r[0].Size != 0x1000 {
		t.Fatalf("pl061 reg: %+v", r)
	}
	if irq := gpio.GICInterrupts(); len(irq) != 1 || irq[0] != 32+7 {
		t.Fatalf("pl061 irq: %v", irq)
	}
	if c := byCompat["arm,pl031"].Strings("compatible"); len(c) != 2 || c[1] != "arm,primecell" {
		t.Fatalf("pl031 compatible: %v", c)
	}
	timer := byCompat["arm,armv8-timer"]
	if irq := timer.GICInterrupts(); len(irq) != 4 || irq[1] != 30 || irq[2] != 27 {
		t.Fatalf("timer PPIs: %v", irq)
	}
	mem := root.Find("/memory@40000000")
	if r := mem.Reg(); len(r) != 1 || r[0].Base != 0x40000000 || r[0].Size != 4<<30 {
		t.Fatalf("memory: %+v", r)
	}
	nvirtio := 0
	root.Walk(func(n *Node) {
		if c := n.Strings("compatible"); len(c) == 1 && c[0] == "virtio,mmio" {
			nvirtio++
		}
	})
	if nvirtio != 32 {
		t.Fatalf("virtio,mmio nodes: %d", nvirtio)
	}
}

func TestRangesTranslation(t *testing.T) {
	// /soc maps bus 0x0 -> CPU 0x2_0000_0000, as on Apple Silicon.
	soc := &Node{Name: "soc", Props: map[string][]byte{
		"#address-cells": be(2), "#size-cells": be(2),
		"ranges": be(0, 0, 0x2, 0, 0x1, 0),
	}}
	root := &Node{Path: "/", Props: map[string][]byte{"#address-cells": be(2), "#size-cells": be(2)}}
	soc.Parent = root
	uart := &Node{Name: "serial@35200000", Parent: soc, Props: map[string][]byte{"reg": be(0, 0x35200000, 0, 0x1000)}}
	if r := uart.Reg(); r[0].Base != 0x2_3520_0000 {
		t.Fatalf("translated %#x", r[0].Base)
	}
}

func TestRejectsGarbage(t *testing.T) {
	if _, err := Parse(make([]byte, 64)); err == nil {
		t.Fatal("accepted zeros")
	}
	b, _ := os.ReadFile("testdata/qemu-virt.dtb")
	if _, err := Parse(b[:100]); err == nil {
		t.Fatal("accepted truncated tree")
	}
}

func be(cells ...uint32) []byte {
	out := make([]byte, 4*len(cells))
	for i, c := range cells {
		out[4*i], out[4*i+1], out[4*i+2], out[4*i+3] = byte(c>>24), byte(c>>16), byte(c>>8), byte(c)
	}
	return out
}
