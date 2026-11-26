package dwc3

import (
	"testing"

	"godeep/kernel/fdt"
)

// tree is the part of this Mac's device tree (Asahi t600x-dieX.dtsi and
// t600x-j314-j316.dtsi) the driver reads, for port 0.
func tree(t *testing.T) *fdt.Node {
	var b fdt.Builder
	b.Begin("")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Begin("soc")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Prop("ranges", nil)

	b.Begin("power-management@28e080000")
	b.PropRegs(2, 2, fdt.Range{Base: 0x2_8e08_0000, Size: 0x4000})
	b.PropU32("#address-cells", 1)
	b.PropU32("#size-cells", 1)
	b.Begin("power-controller@2b0")
	b.PropStrings("compatible", "apple,t6000-pmgr-pwrstate", "apple,pmgr-pwrstate")
	b.PropU32("reg", 0x2b0, 8)
	b.PropU32("#power-domain-cells", 0)
	b.PropU32("phandle", 1)
	b.End()
	b.End()

	b.Begin("usb@702280000")
	b.PropStrings("compatible", "apple,t6000-dwc3", "apple,t8103-dwc3")
	b.PropRegs(2, 2, fdt.Range{Base: 0x7_0228_0000, Size: 0xcd00}, fdt.Range{Base: 0x7_0228_cd00, Size: 0x3200})
	b.PropU32("iommus", 2, 0, 3, 1)
	b.PropU32("power-domains", 1)
	b.PropU32("phys", 4, 0, 4, 1)
	b.Begin("ports")
	b.Begin("port@0")
	b.Begin("endpoint")
	b.PropU32("remote-endpoint", 20)
	b.PropU32("phandle", 21)
	b.End()
	b.End()
	b.End()
	b.End()
	for i, base := range []uint64{0x7_02f0_0000, 0x7_02f8_0000} {
		b.Begin("iommu@" + []string{"702f00000", "702f80000"}[i])
		b.PropStrings("compatible", "apple,t6000-dart")
		b.PropRegs(2, 2, fdt.Range{Base: base, Size: 0x4000})
		b.PropU32("#iommu-cells", 1)
		b.PropU32("power-domains", 1)
		b.PropU32("phandle", uint32(2+i))
		b.End()
	}
	b.Begin("phy@703000000")
	b.PropStrings("compatible", "apple,t6000-atcphy", "apple,t8103-atcphy")
	b.PropRegs(2, 2, fdt.Range{Base: 0x7_0300_0000, Size: 0x4c000}, fdt.Range{Base: 0x7_0305_0000, Size: 0x8000},
		fdt.Range{Base: 0x7_0000_0000, Size: 0x4000}, fdt.Range{Base: 0x7_02a9_0000, Size: 0x4000},
		fdt.Range{Base: 0x7_02a8_4000, Size: 0x4000})
	b.PropStrings("reg-names", "core", "lpdptx", "axi2af", "usb2phy", "pipehandler")
	for _, n := range []string{"axi2af", "common-a", "common-b", "lane0-usb", "lane1-usb", "lane0-dp", "lane1-dp"} {
		b.PropU32("apple,tunable-"+n, 0x10, 0xff, 0x42, 0x14, 0x1, 0x1)
	}
	b.PropU32("#phy-cells", 1)
	b.PropU32("power-domains", 1)
	b.PropU32("phandle", 4)
	b.End()

	b.Begin("i2c@39b040000")
	b.PropStrings("compatible", "apple,t6000-i2c", "apple,i2c")
	b.PropRegs(2, 2, fdt.Range{Base: 0x3_9b04_0000, Size: 0x4000})
	b.PropU32("#address-cells", 1)
	b.PropU32("#size-cells", 0)
	b.Begin("usb-pd@38")
	b.PropStrings("compatible", "apple,cd321x")
	b.PropU32("reg", 0x38)
	b.Begin("connector")
	b.PropStrings("compatible", "usb-c-connector")
	b.PropStrings("label", "USB-C Left Back")
	b.Begin("ports")
	b.Begin("port@0")
	b.Begin("endpoint")
	b.PropU32("remote-endpoint", 21)
	b.PropU32("phandle", 20)
	b.End()
	b.End()
	b.End()
	b.End()
	b.End()
	b.End()

	b.End() // soc
	b.End()
	blob, err := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	root, err := fdt.Parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestConfigFromFDT(t *testing.T) {
	root := tree(t)
	c, err := ConfigFromFDT(root.Find("/soc/usb@702280000"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Core != 0x7_0228_0000 || c.USB2PHY != 0x7_02a9_0000 || c.Pipehandler != 0x7_02a8_4000 {
		t.Fatalf("windows: %s", c.Summary())
	}
	if len(c.DARTs) != 2 || c.DARTs[0] != (Stream{0x7_02f0_0000, 0}) || c.DARTs[1] != (Stream{0x7_02f8_0000, 1}) {
		t.Fatalf("DARTs: %+v", c.DARTs)
	}
	if len(c.Power) != 1 || c.Power[0].Addr != 0x2_8e08_02b0 {
		t.Fatalf("power: %+v", c.Power)
	}
	if c.ATC != 0x7_0300_0000 || c.AXI2AF != 0x7_0000_0000 || c.Tunables == nil ||
		len(c.Tunables.LaneDP[1]) != 2 || c.Tunables.CommonA[0] != (Tunable{0x10, 0xff, 0x42}) {
		t.Fatalf("USB 3: atc %#x axi2af %#x tunables %+v", c.ATC, c.AXI2AF, c.Tunables)
	}
	if c.I2C != "i2c@39b040000" || c.HPM != 0x38 || c.Label != "USB-C Left Back" {
		t.Fatalf("port controller: %s", c.Summary())
	}
}
