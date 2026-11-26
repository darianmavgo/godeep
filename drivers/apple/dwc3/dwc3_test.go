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
	if err := p.up(context.Background(), true); err == nil {
		t.Fatal("came up without a DWC3 core")
	}
	core.Set(GSNPSID, 0x3331_330b)
	core.Set(GCTL, 2<<12) // device mode, as m1n1's gadget left it
	if err := p.up(context.Background(), true); err != nil {
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

// The USB 3 bring-up against a PHY that acks everything: lanes and
// crossbar for each orientation, tunables applied, the PIPE mux switched.
func TestUSB3Lanes(t *testing.T) {
	for _, swap := range []bool{false, true} {
		core, axi, pipe := mmio.NewFake(), mmio.NewFake(), mmio.NewFake()
		core.OnRead = func(f *mmio.Fake, off uint64) (uint32, bool) {
			switch off {
			case ATCPHY_POWER_STAT: // mirrors the requested sleep states
				return f.Get(ATCPHY_POWER_CTRL) & 3, true
			case ACIOPHY_TOP_PHY_STAT:
				return statLn0Unk0, true
			}
			return 0, false
		}
		pipe.OnRead = func(f *mmio.Fake, off uint64) (uint32, bool) {
			if off == PIPE_LOCK_ACK {
				return f.Get(PIPE_LOCK_REQ) & 1, true
			}
			return 0, false
		}
		tun := &Tunables{CommonA: []Tunable{{0x100, 0xff, 0x12}}, AXI2AF: []Tunable{{0x8, 0xf, 0x3}},
			LaneUSB: [2][]Tunable{{{0x200, 1, 1}}, {{0x204, 1, 1}}}, LaneDP: [2][]Tunable{{{0x300, 1, 1}}, {{0x304, 1, 1}}}}
		a := &atc{core: core, axi2af: axi, pipe: pipe, t: tun}
		if err := a.powerOn(swap); err != nil {
			t.Fatal(err)
		}
		lm, xb := core.Get(ACIOPHY_LANE_MODE), core.Get(ACIOPHY_CROSSBAR)
		usbLane, dpLane := uint64(0x200), uint64(0x304)
		if swap {
			if lm != 2|2<<3|1<<6|1<<9 || xb != 0x11|0x8<<5 || core.Get(ATCPHY_MISC)&MISC_LANE_SWAP == 0 {
				t.Fatalf("swapped: lane mode %#x crossbar %#x misc %#x", lm, xb, core.Get(ATCPHY_MISC))
			}
			usbLane, dpLane = 0x204, 0x300
		} else if lm != 1|1<<3|2<<6|2<<9 || xb != 0x10|0x8<<5 {
			t.Fatalf("normal: lane mode %#x crossbar %#x", lm, xb)
		}
		if core.Get(0x100) != 0x12 || axi.Get(0x8) != 3 || core.Get(usbLane) != 1 || core.Get(dpLane) != 1 {
			t.Fatal("tunables not applied to the right lanes")
		}
		if core.Get(ATCPHY_POWER_CTRL)&POWER_PHY_RESET_N == 0 {
			t.Fatal("PHY still in reset")
		}
		if err := a.pipeUSB3Host(); err != nil {
			t.Fatal(err)
		}
		if pipe.Get(PIPE_MUX_CTRL) != pipeMuxUSB3 || pipe.Get(PIPE_LOCK_REQ) != 0 {
			t.Fatalf("mux %#x lock %#x", pipe.Get(PIPE_MUX_CTRL), pipe.Get(PIPE_LOCK_REQ))
		}
		a.pipeDummy()
		a.powerOff()
		if pipe.Get(PIPE_MUX_CTRL) != PIPE_MUX_DUMMY || core.Get(ATCPHY_POWER_CTRL)&(POWER_SLEEP_BIG|POWER_SLEEP_SMALL) != 0 {
			t.Fatal("not back to dummy / asleep")
		}
	}
}

// A BIST that never completes leaves the port on USB 2.
func TestUSB3BISTTimeout(t *testing.T) {
	core, pipe := mmio.NewFake(), mmio.NewFake()
	pipe.OnRead = func(f *mmio.Fake, off uint64) (uint32, bool) {
		if off == PIPE_LOCK_ACK {
			return f.Get(PIPE_LOCK_REQ) & 1, true
		}
		return 0, false
	}
	a := &atc{core: core, axi2af: mmio.NewFake(), pipe: pipe, t: &Tunables{}}
	if a.pipeUSB3Host() == nil {
		t.Fatal("BIST timeout not reported")
	}
	if pipe.Get(PIPE_MUX_CTRL) == pipeMuxUSB3 {
		t.Fatal("switched to USB 3 after a failed BIST")
	}
}
