package pmgr

import (
	"context"
	"testing"

	"godeep/kernel/fdt"
	"godeep/kernel/mmio"
)

// fakeDomain models a domain register: a target write takes effect at once.
func fakeDomain(initial uint32, stuck bool) *mmio.Fake {
	f := mmio.NewFake()
	f.Set(0, initial)
	f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		if v&(WAS_CLKGATED|WAS_PWRGATED) != 0 {
			panic("wrote a W1C bit")
		}
		keep := f.Get(0) & (WAS_CLKGATED | WAS_PWRGATED)
		if !stuck {
			v = v&^PS_ACTUAL | (v&PS_TARGET)<<4
		} else {
			v = v&^PS_ACTUAL | f.Get(0)&PS_ACTUAL
		}
		f.Set(0, v|keep)
		return true
	}
	return f
}

func TestOn(t *testing.T) {
	ctx := context.Background()

	on := fakeDomain(0xff, false)
	if sw, err := On(ctx, on); err != nil || sw {
		t.Fatalf("already on: switched=%v err=%v", sw, err)
	}
	if len(mmio.Writes(on.Log())) != 0 {
		t.Fatal("wrote to a domain that was already on")
	}

	off := fakeDomain(RESET|WAS_PWRGATED|0x00, false)
	if sw, err := On(ctx, off); err != nil || !sw {
		t.Fatalf("off: switched=%v err=%v", sw, err)
	}
	if v := off.Get(0); v&PS_ACTUAL != 0xf0 || v&RESET != 0 {
		t.Fatalf("register %#x", v)
	}

	stuck := fakeDomain(0x00, true)
	if _, err := On(ctx, stuck); err == nil {
		t.Fatal("no error for a domain that never turns on")
	}
}

func TestFromFDT(t *testing.T) {
	var b fdt.Builder
	b.Begin("")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Begin("soc")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Prop("ranges", nil)
	b.Begin("power-management@28e080000")
	b.PropStrings("compatible", "apple,t6000-pmgr", "apple,pmgr", "syscon", "simple-mfd")
	b.PropRegs(2, 2, fdt.Range{Base: 0x2_8e08_0000, Size: 0x4000})
	b.PropU32("#address-cells", 1)
	b.PropU32("#size-cells", 1)
	dom := func(name string, ph, off uint32, parents ...uint32) {
		b.Begin(name)
		b.PropStrings("compatible", "apple,t6000-pmgr-pwrstate", "apple,pmgr-pwrstate")
		b.PropU32("reg", off, 4)
		b.PropU32("phandle", ph)
		b.PropU32("#power-domain-cells", 0)
		if len(parents) > 0 {
			b.PropU32("power-domains", parents...)
		}
		b.End()
	}
	dom("power-controller@1d8", 1, 0x1d8)
	dom("power-controller@180", 2, 0x180, 1)
	dom("power-controller@1e8", 3, 0x1e8, 2)
	dom("power-controller@208", 4, 0x208, 3)
	dom("power-controller@120", 5, 0x120, 1, 2) // shares parents
	b.End()
	b.Begin("spi@39b10c000")
	b.PropU32("power-domains", 4, 5)
	b.End()
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
	got, err := FromFDT(root.Find("/soc/spi@39b10c000"))
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{0x2_8e08_01d8, 0x2_8e08_0180, 0x2_8e08_01e8, 0x2_8e08_0208, 0x2_8e08_0120}
	if len(got) != len(want) {
		t.Fatalf("domains: %+v", got)
	}
	for i := range want {
		if got[i].Addr != want[i] {
			t.Fatalf("domain %d: %+v, want %#x", i, got[i], want[i])
		}
	}
}
