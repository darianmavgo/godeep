package aic

import (
	"testing"

	"godeep/kernel/mmio"
)

func TestGeometryMaskAndEvent(t *testing.T) {
	core, ev := mmio.NewFake(), mmio.NewFake()
	core.Set(INFO1, 1<<24|0x0400) // 2 dies, 1024 IRQs
	core.Set(INFO3, 0x0400)
	a, err := New(core, ev)
	if err != nil {
		t.Fatal(err)
	}
	// IRQ_CFG 4 KB, then SW_SET, SW_CLR, MASK_SET, MASK_CLR, HW_STATE of 128 B
	if a.maskSet != 0x2000+0x1000+0x100 || a.maskClr != a.maskSet+0x80 || a.stride != 0x1000+5*0x80 {
		t.Fatalf("offsets: maskSet %#x maskClr %#x stride %#x", a.maskSet, a.maskClr, a.stride)
	}
	a.Init()
	w := mmio.Writes(core.Log())
	if len(w) != 2*2*32+1 || core.Get(CONFIG)&CONFIG_ENABLE == 0 {
		t.Fatalf("%d init writes, CONFIG %#x", len(w), core.Get(CONFIG))
	}
	a.Enable(1<<24 | 1069) // die 1, irq 1069 = bank 33, bit 13
	w = mmio.Writes(core.Log())
	if len(w) != 1 || w[0].Off != a.stride+a.maskClr+4*33 || w[0].Val != 1<<13 {
		t.Fatalf("Enable wrote %v", w)
	}
	a.Disable(5)
	w = mmio.Writes(core.Log())
	if len(w) != 1 || w[0].Off != a.maskSet || w[0].Val != 1<<5 {
		t.Fatalf("Disable wrote %v", w)
	}
	ev.Set(0, 1<<24|EventIRQ<<16|754)
	if k, id := a.Event(); k != EventIRQ || id != 1<<24|754 {
		t.Fatalf("event %d %#x", k, id)
	}
	if _, err := New(mmio.NewFake(), ev); err == nil {
		t.Fatal("accepted an all-zero controller")
	}
}
