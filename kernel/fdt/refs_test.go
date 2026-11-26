package fdt

import "testing"

func TestRefs(t *testing.T) {
	var b Builder
	b.Begin("")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Begin("gpio@100")
	b.PropStrings("compatible", "apple,t6000-pinctrl", "apple,pinctrl")
	b.PropU32("phandle", 7)
	b.PropU32("#gpio-cells", 2)
	b.End()
	b.Begin("clk")
	b.PropU32("phandle", 9)
	b.End()
	b.Begin("dev")
	b.PropU32("enable-gpios", 7, 194, 0, 9)
	b.PropU32("broken-gpios", 7, 1)
	b.PropU32("dangling", 42)
	b.End()
	b.End()
	dtb, err := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	root, err := Parse(dtb)
	if err != nil {
		t.Fatal(err)
	}
	dev := root.Find("/dev")
	refs, err := dev.Refs("enable-gpios", "#gpio-cells")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].Node.Name != "gpio@100" || len(refs[0].Args) != 2 || refs[0].Args[0] != 194 ||
		refs[1].Node.Name != "clk" || len(refs[1].Args) != 0 {
		t.Fatalf("refs: %+v", refs)
	}
	if !refs[0].Node.Compatible("apple,pinctrl") || refs[0].Node.Compatible("apple,spi") {
		t.Fatal("Compatible")
	}
	if _, err := dev.Refs("broken-gpios", "#gpio-cells"); err == nil {
		t.Fatal("truncated list accepted")
	}
	if _, err := dev.Refs("dangling", "#gpio-cells"); err == nil {
		t.Fatal("dangling phandle accepted")
	}
	if r, _ := dev.Refs("missing", "#gpio-cells"); len(r) != 0 {
		t.Fatal("missing property")
	}
	if dev.Root() != root || root.ByPhandle(0) != nil {
		t.Fatal("Root/ByPhandle")
	}
}
