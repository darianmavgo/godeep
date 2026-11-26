package dwc3

import (
	"errors"
	"fmt"
	"time"

	"godeep/kernel/mmio"
)

// The Type-C PHY's SuperSpeed half (Linux's phy-apple-atc.c, USB 3 host
// mode on t6000 only: no DisplayPort, USB4 or device mode). Per cable:
// power the PHY up, apply the tunables m1n1 copies into the device tree,
// set the lanes and crossbar for the plug's orientation, take the PHY out
// of reset; after the core is up, run the pipehandler's BIST sequence and
// switch dwc3's PIPE interface from the dummy PHY to the USB 3 PHY.

// Core registers.
const (
	AUSPLL_FSM_CTRL         = 0x1014
	AUSPLL_APB_CMD_OVERRIDE = 0x2000
	CIO3PLL_CLK_CTRL        = 0x2a00
	ACIOPHY_CFG0            = 0x08
	ACIOPHY_LANE_MODE       = 0x48
	ACIOPHY_CROSSBAR        = 0x4c
	ACIOPHY_BIST_CIOPHY1    = 0x84
	ACIOPHY_BIST_OV_CFG     = 0x8c
	ACIOPHY_BIST_READ_CTRL  = 0x90
	ACIOPHY_TOP_PHY_STAT    = 0x9c
	ACIOPHY_BIST_PHY_CFG0   = 0xa8
	ACIOPHY_BIST_PHY_CFG1   = 0xac
	ACIOPHY_SLEEP_CTRL      = 0x1b0
	ATCPHY_POWER_CTRL       = 0x20000
	ATCPHY_POWER_STAT       = 0x20004
	ATCPHY_MISC             = 0x20008
	LN0_RX_PMAFSM           = 0x9000 + 0x10
	LN1_RX_PMAFSM           = 0x10000 + 0x10

	POWER_SLEEP_SMALL = 1 << 0
	POWER_SLEEP_BIG   = 1 << 1
	POWER_CLAMP_EN    = 1 << 2
	POWER_APB_RESET_N = 1 << 3
	POWER_PHY_RESET_N = 1 << 4
	MISC_RESET_N      = 1 << 0
	MISC_LANE_SWAP    = 1 << 2
	PMAFSM_PCS_OV     = 1 << 0
	PMAFSM_PCS_REQ    = 1 << 9

	laneUSB3, laneDP           = 1, 2
	crossbarUSB3DP             = 0x10
	crossbarUSB3DPSwapped      = 0x11
	crossbarSinglePMA          = 0x008 << 5
	crossbarMask               = 0x1f | 0xfff<<5 | 1<<17
	laneModeMask               = 0xfff
	statLn0Unk0, statLn0Unk23  = 1 << 0, 1 << 23
	bistClkEn, bistEn          = 1 << 27, 1 << 28
	bistOvResetN, bistOvPwrDn  = 1 << 13, 1 << 25
	bistReadPhyStatus          = 1 << 2
	bistLn0ResetN              = 1 << 0
	bistLn0PwrDownMask         = 0xf << 10
	PIPE_OVERRIDE              = 0x00
	PIPE_OVERRIDE_VALUES       = 0x04
	PIPE_LOCK_REQ              = 0x10
	PIPE_LOCK_ACK              = 0x14
	pipeRxValid, pipeRxDetect  = 1 << 0, 1 << 2
	pipeValRxDet0, pipeValRxD1 = 1 << 1, 1 << 2
	pipeMuxUSB3                = 1<<3 | 0 // clock USB3 (1), data USB3 (0)
	pipeNativePowerDown        = 0xf
	pipeNativeReset            = 1 << 12
)

// Tunable is one masked register write from the boot loader.
type Tunable struct{ Off, Mask, Val uint32 }

// Tunables are the "apple,tunable-*" properties m1n1 fills in for each
// Type-C PHY (Linux's atcphy_load_tunables).
type Tunables struct {
	AXI2AF, CommonA, CommonB []Tunable
	LaneUSB, LaneDP          [2][]Tunable
}

func apply(r mmio.Regs, ts []Tunable) {
	for _, t := range ts {
		old := r.Read32(uint64(t.Off))
		if v := old&^t.Mask | t.Val; v != old {
			r.Write32(uint64(t.Off), v)
		}
	}
}

func waitBits(r mmio.Regs, off uint64, mask, want uint32, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for r.Read32(off)&mask != want {
		if time.Now().After(deadline) {
			return fmt.Errorf("register %#x: %#x, waiting for %#x/%#x", off, r.Read32(off), want, mask)
		}
		time.Sleep(10 * time.Microsecond)
	}
	return nil
}

// atc is a port's Type-C PHY for USB 3.
type atc struct {
	core, axi2af, pipe mmio.Regs
	t                  *Tunables
}

// powerOn wakes the PHY and configures it for USB 3 (plus a DP lane, as
// Linux does: USB3/USB3 doesn't work) in the plug's orientation.
func (a *atc) powerOn(swap bool) error {
	c := a.core
	mmio.Modify(c, ATCPHY_MISC, 0, MISC_RESET_N)
	mmio.Modify(c, ATCPHY_POWER_CTRL, 0, POWER_SLEEP_SMALL)
	if err := waitBits(c, ATCPHY_POWER_STAT, POWER_SLEEP_SMALL, POWER_SLEEP_SMALL, 100*time.Millisecond); err != nil {
		return fmt.Errorf("atc: wake small: %w", err)
	}
	mmio.Modify(c, ATCPHY_POWER_CTRL, 0, POWER_SLEEP_BIG)
	if err := waitBits(c, ATCPHY_POWER_STAT, POWER_SLEEP_BIG, POWER_SLEEP_BIG, 100*time.Millisecond); err != nil {
		return fmt.Errorf("atc: wake big: %w", err)
	}
	mmio.Modify(c, ATCPHY_POWER_CTRL, POWER_CLAMP_EN, 0)
	mmio.Modify(c, ATCPHY_POWER_CTRL, 0, POWER_APB_RESET_N)

	lane0, lane1 := 0, 1
	if swap {
		lane0, lane1 = 1, 0
	}
	apply(c, a.t.CommonA)
	apply(a.axi2af, a.t.AXI2AF)
	apply(c, a.t.CommonB)
	apply(c, a.t.LaneUSB[lane0])
	apply(c, a.t.LaneDP[lane1])

	// post-tunable (t8103 generation, which t6000 is)
	mmio.Modify(c, AUSPLL_FSM_CTRL, 0, 0x1fe000)
	mmio.Modify(c, AUSPLL_APB_CMD_OVERRIDE, 0, 1<<28)
	for _, b := range []uint32{1 << 3, 1 << 1, 1 << 5} { // common small, big, clamp overrides
		mmio.Modify(c, ACIOPHY_CFG0, 0, b)
		udelay()
	}
	for _, sh := range []uint{6, 2, 10} { // TX small, big, clamp overrides
		mmio.Modify(c, ACIOPHY_SLEEP_CTRL, 3<<sh, 3<<sh)
		udelay()
	}
	for _, sh := range []uint{12, 8, 16} { // RX big, small, clamp overrides
		mmio.Modify(c, ACIOPHY_CFG0, 3<<sh, 3<<sh)
		udelay()
	}

	mmio.Modify(c, CIO3PLL_CLK_CTRL, 0, 1<<1) // PCLK
	mmio.Modify(c, CIO3PLL_CLK_CTRL, 0, 1<<5) // REFCLK

	// lanes: USB 3 on the plug's lane, DP on the other
	m0, m1, xbar := uint32(laneUSB3), uint32(laneDP), uint32(crossbarUSB3DP)
	if swap {
		m0, m1, xbar = laneDP, laneUSB3, crossbarUSB3DPSwapped
	}
	mmio.Modify(c, ACIOPHY_LANE_MODE, laneModeMask, m0|m0<<3|m1<<6|m1<<9)
	mmio.Modify(c, ACIOPHY_CROSSBAR, crossbarMask, xbar|crossbarSinglePMA)
	if swap {
		mmio.Modify(c, ATCPHY_MISC, 0, MISC_LANE_SWAP)
	} else {
		mmio.Modify(c, ATCPHY_MISC, MISC_LANE_SWAP, 0)
	}
	dp := [2]bool{!swap, swap} // which lane carries DP
	for i, off := range []uint64{LN0_RX_PMAFSM, LN1_RX_PMAFSM} {
		if dp[i] {
			mmio.Modify(c, off, 0, PMAFSM_PCS_OV)
			udelay()
			mmio.Modify(c, off, PMAFSM_PCS_REQ, 0)
		} else {
			mmio.Modify(c, off, PMAFSM_PCS_OV, 0)
			udelay()
		}
	}
	mmio.Modify(c, ATCPHY_POWER_CTRL, 0, POWER_PHY_RESET_N)
	return nil
}

// powerOff puts the PHY back to sleep (Linux's atcphy_power_off).
func (a *atc) powerOff() {
	c := a.core
	mmio.Modify(c, ATCPHY_POWER_CTRL, POWER_PHY_RESET_N, 0)
	mmio.Modify(c, ATCPHY_POWER_CTRL, 0, POWER_CLAMP_EN)
	mmio.Modify(c, ATCPHY_MISC, MISC_RESET_N|MISC_LANE_SWAP, 0)
	mmio.Modify(c, ATCPHY_POWER_CTRL, POWER_APB_RESET_N, 0)
	mmio.Modify(c, ATCPHY_POWER_CTRL, POWER_SLEEP_BIG, 0)
	waitBits(c, ATCPHY_POWER_STAT, POWER_SLEEP_BIG, 0, time.Millisecond)
	mmio.Modify(c, ATCPHY_POWER_CTRL, POWER_SLEEP_SMALL, 0)
	waitBits(c, ATCPHY_POWER_STAT, POWER_SLEEP_SMALL, 0, time.Millisecond)
}

func (a *atc) lock() error {
	if a.pipe.Read32(PIPE_LOCK_REQ)&1 != 0 {
		return nil
	}
	mmio.Modify(a.pipe, PIPE_LOCK_REQ, 0, 1)
	if err := waitBits(a.pipe, PIPE_LOCK_ACK, 1, 1, time.Millisecond); err != nil {
		mmio.Modify(a.pipe, PIPE_LOCK_REQ, 1, 0)
		return errors.New("atc: pipehandler lock not acked")
	}
	return nil
}

func (a *atc) unlock() error {
	mmio.Modify(a.pipe, PIPE_LOCK_REQ, 1, 0)
	return waitBits(a.pipe, PIPE_LOCK_ACK, 1, 0, time.Millisecond)
}

func (a *atc) setMux(v uint32) {
	mmio.Modify(a.pipe, PIPE_MUX_CTRL, 7<<3, 0) // clock off
	udelay()
	mmio.Modify(a.pipe, PIPE_MUX_CTRL, 7, v&7) // data
	udelay()
	mmio.Modify(a.pipe, PIPE_MUX_CTRL, 7<<3, v&(7<<3)) // clock
	udelay()
}

// pipeUSB3Host switches dwc3's PIPE interface to the USB 3 PHY, with the
// BIST sequence host mode needs (Linux's atcphy_configure_pipehandler_usb3
// and _usb3_host_t8103). Timeouts inside the BIST are warnings in Linux;
// here they're returned, and the caller stays on USB 2.
func (a *atc) pipeUSB3Host() error {
	p, c := a.pipe, a.core
	if p.Read32(PIPE_LOCK_ACK)&1 != 0 {
		a.unlock()
	}
	mmio.Modify(p, PIPE_OVERRIDE_VALUES, pipeValRxDet0|pipeValRxD1, 0)
	mmio.Modify(p, PIPE_OVERRIDE, 0, pipeRxValid)
	mmio.Modify(p, PIPE_OVERRIDE, 0, pipeRxDetect)
	if err := a.lock(); err != nil {
		return err
	}
	var bistErr error
	note := func(err error) {
		if err != nil && bistErr == nil {
			bistErr = err
		}
	}
	mmio.Modify(c, ACIOPHY_BIST_PHY_CFG0, 0, bistLn0ResetN)
	mmio.Modify(c, ACIOPHY_BIST_OV_CFG, 0, bistOvResetN)
	note(waitBits(c, ACIOPHY_TOP_PHY_STAT, statLn0Unk23, 0, 10*time.Millisecond))
	mmio.Modify(c, ACIOPHY_BIST_READ_CTRL, 0, bistReadPhyStatus)
	mmio.Modify(c, ACIOPHY_BIST_READ_CTRL, bistReadPhyStatus, 0)
	mmio.Modify(c, ACIOPHY_BIST_PHY_CFG1, bistLn0PwrDownMask, 3<<10)
	mmio.Modify(c, ACIOPHY_BIST_OV_CFG, 0, bistOvPwrDn)
	mmio.Modify(c, ACIOPHY_BIST_CIOPHY1, 0, bistClkEn)
	mmio.Modify(c, ACIOPHY_BIST_CIOPHY1, 0, bistEn)
	c.Write32(ACIOPHY_BIST_CIOPHY1, 0)
	note(waitBits(c, ACIOPHY_TOP_PHY_STAT, statLn0Unk0, statLn0Unk0, 10*time.Millisecond))
	note(waitBits(c, ACIOPHY_TOP_PHY_STAT, statLn0Unk23, 0, 10*time.Millisecond))
	// clear the non-selected PHY's reset
	mmio.Modify(p, PIPE_NONSEL_OVR, pipeNativePowerDown, 3)
	mmio.Modify(p, PIPE_NONSEL_OVR, pipeNativeReset, 0)
	c.Write32(ACIOPHY_BIST_OV_CFG, 0)
	mmio.Modify(c, ACIOPHY_BIST_CIOPHY1, 0, bistClkEn)
	mmio.Modify(c, ACIOPHY_BIST_CIOPHY1, 0, bistEn)

	if bistErr == nil {
		a.setMux(pipeMuxUSB3)
	}
	mmio.Modify(p, PIPE_OVERRIDE, pipeRxValid, 0)
	mmio.Modify(p, PIPE_OVERRIDE, pipeRxDetect, 0)
	if err := a.unlock(); err != nil && bistErr == nil {
		bistErr = errors.New("atc: pipehandler unlock not acked")
	}
	if bistErr != nil {
		return fmt.Errorf("atc: USB 3 BIST: %w", bistErr)
	}
	return nil
}

// pipeDummy puts the PIPE interface back on the dummy PHY.
func (a *atc) pipeDummy() {
	p := a.pipe
	if p.Read32(PIPE_LOCK_ACK)&1 != 0 {
		a.unlock()
	}
	mmio.Modify(p, PIPE_OVERRIDE_VALUES, pipeValRxDet0|pipeValRxD1, 0)
	mmio.Modify(p, PIPE_OVERRIDE, 0, pipeRxValid)
	mmio.Modify(p, PIPE_OVERRIDE, 0, pipeRxDetect)
	a.lock()
	a.setMux(PIPE_MUX_DUMMY)
	a.unlock()
	mmio.Modify(p, PIPE_NONSEL_OVR, pipeNativePowerDown, 2)
	mmio.Modify(p, PIPE_NONSEL_OVR, 0, pipeNativeReset)
}
