package dwc3

import (
	"context"
	"testing"

	"godeep/kernel/mmio"
)

func TestUpAndReset(t *testing.T) {
	p := &port{core: mmio.NewFake(), phy: mmio.NewFake(), pipe: mmio.NewFake()}
	core, phy, pipe := p.core.(*mmio.Fake), p.phy.(*mmio.Fake), p.pipe.(*mmio.Fake)
	p.resetAssert()
	if v := pipe.Get(PIPE_AON_GEN); v&PIPE_DWC3_RESET_N != 0 || v&PIPE_DWC3_CLAMP_EN == 0 {
		t.Fatalf("core not held in reset: AON_GEN %#x", v)
	}
	if pipe.Get(PIPE_MUX_CTRL) != PIPE_MUX_DUMMY {
		t.Fatal("PIPE not on the dummy PHY")
	}
	if v := phy.Get(USB2PHY_CTL); v&(CTL_RESET|CTL_PORT_RESET|CTL_SIDDQ) != CTL_RESET|CTL_PORT_RESET|CTL_SIDDQ {
		t.Fatalf("USB 2 PHY not down: CTL %#x", v)
	}
	if err := p.up(context.Background()); err == nil {
		t.Fatal("came up without a DWC3 core")
	}
	core.Set(GSNPSID, 0x3331_330b)
	core.Set(GCTL, 2<<12) // device mode, as m1n1's gadget left it
	if err := p.up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := pipe.Get(PIPE_AON_GEN); v&PIPE_DWC3_RESET_N == 0 || v&PIPE_DWC3_CLAMP_EN != 0 {
		t.Fatalf("core still in reset: AON_GEN %#x", v)
	}
	if v := phy.Get(USB2PHY_SIG); v&SIG_HOST != SIG_HOST {
		t.Fatalf("PHY not in host mode: SIG %#x", v)
	}
	if v := phy.Get(USB2PHY_CTL); v&(CTL_RESET|CTL_PORT_RESET|CTL_SIDDQ) != 0 || v&CTL_APB_RESET_N == 0 {
		t.Fatalf("USB 2 PHY not up: CTL %#x", v)
	}
	if phy.Get(USB2PHY_USBCTL) != USBCTL_RUN {
		t.Fatal("USB 2 PHY not running")
	}
	if v := core.Get(GCTL); v&GCTL_PRTCAPDIR_MASK != GCTL_PRTCAP_HOST || v&GCTL_CORESOFTRESET != 0 {
		t.Fatalf("GCTL %#x", v)
	}
	if core.Get(GUSB2PHYCFG)&(USB2PHY_SUSPHY|PHYSOFTRST) != USB2PHY_SUSPHY {
		t.Fatalf("GUSB2PHYCFG %#x", core.Get(GUSB2PHYCFG))
	}
}
