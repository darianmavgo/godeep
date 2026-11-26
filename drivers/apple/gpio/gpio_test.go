package gpio

import (
	"testing"

	"godeep/kernel/fdt"
	"godeep/kernel/mmio"
)

func TestPins(t *testing.T) {
	f := mmio.NewFake()
	b := Block{f}
	const pull = 3 << 7
	f.Set(reg(194), pull|1<<5) // muxed, pulled up
	b.Set(194, true)
	if v := f.Get(reg(194)); v != pull|MODE_OUT<<1|DATA {
		t.Fatalf("output high: %#x", v)
	}
	b.Set(194, false)
	if v := f.Get(reg(194)); v != pull|MODE_OUT<<1 {
		t.Fatalf("output low: %#x", v)
	}

	if b.IsInput(6) {
		t.Fatal("unconfigured pin reported as input")
	}
	b.Input(6)
	if !b.IsInput(6) || f.Get(reg(6)) != MODE_IN_IRQ_OFF<<1|INPUT_ENABLE {
		t.Fatalf("input: %#x", f.Get(reg(6)))
	}
	f.Set(reg(6), f.Get(reg(6))|DATA)
	if !b.Get(6) {
		t.Fatal("level not read")
	}

	f.Set(reg(52), 1<<5)
	f.Log()
	if b.Mux(52, 1) || len(mmio.Writes(f.Log())) != 0 {
		t.Fatal("rewrote a pin already muxed")
	}
	if !b.Mux(53, 1) || f.Get(reg(53)) != 1<<5|INPUT_ENABLE {
		t.Fatalf("mux: %#x", f.Get(reg(53)))
	}
}

func TestFromFDT(t *testing.T) {
	var b fdt.Builder
	b.Begin("")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Begin("pinctrl@39b028000")
	b.PropStrings("compatible", "apple,t6000-pinctrl", "apple,pinctrl")
	b.PropRegs(2, 2, fdt.Range{Base: 0x3_9b02_8000, Size: 0x4000})
	b.PropU32("apple,npins", 255)
	b.PropU32("#gpio-cells", 2)
	b.PropU32("phandle", 1)
	b.Begin("spi3-pins")
	b.PropU32("pinmux", 52|1<<16, 53|1<<16)
	b.PropU32("phandle", 2)
	b.End()
	b.End()
	b.Begin("dev")
	b.PropU32("pinctrl-0", 2)
	b.PropU32("spien-gpios", 1, 194, 0)
	b.PropU32("bad-gpios", 1, 300, 0)
	b.End()
	b.End()
	dtb, err := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	root, err := fdt.Parse(dtb)
	if err != nil {
		t.Fatal(err)
	}
	dev := root.Find("/dev")
	m, err := MuxFromFDT(dev)
	if err != nil || len(m) != 2 || m[1].Pin.Pin != 53 || m[1].Func != 1 || m[1].Block != 0x3_9b02_8000 {
		t.Fatalf("mux %+v %v", m, err)
	}
	refs, _ := dev.Refs("spien-gpios", "#gpio-cells")
	p, err := PinFromRef(refs[0])
	if err != nil || p.Pin != 194 || p.Block != 0x3_9b02_8000 {
		t.Fatalf("pin %+v %v", p, err)
	}
	refs, _ = dev.Refs("bad-gpios", "#gpio-cells")
	if _, err := PinFromRef(refs[0]); err == nil {
		t.Fatal("pin 300 accepted on a 255-pin block")
	}
}
