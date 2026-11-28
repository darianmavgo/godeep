package agx

import (
	"fmt"
	"math"

	"godeep/kernel/fdt"
)

// PState is one GPU performance state (an operating point).
type PState struct {
	FreqHz uint32
	VoltMV []uint32 // per cluster
	PwrMW  uint32
}

// MaxVoltMV is the highest voltage of the state.
func (p PState) MaxVoltMV() uint32 {
	m := uint32(0)
	for _, v := range p.VoltMV {
		m = max(m, v)
	}
	return m
}

// PowerZone is one "apple,power-zones" entry.
type PowerZone struct{ Target, TargetOffset, FilterTC uint32 }

// PwrConfig is the GPU's power management configuration from the device
// tree (Asahi: hw::PwrConfig::load). Most names are Apple's ADT property
// names; they tune the firmware's power and performance control loops.
type PwrConfig struct {
	PerfStates []PState
	PowerZones []PowerZone
	CoreLeak   []float32
	SramLeak   []float32

	MaxPowerMW, MaxFreqMHz        uint32
	PerfBasePState, PerfMaxPState uint32
	MinSramMicrovolt              uint32

	AvgPowerFilterTCms       uint32
	AvgPowerKiOnly           float32
	AvgPowerKp               float32
	AvgPowerMinDutyCycle     uint32
	AvgPowerTargetFilterTC   uint32
	FastDie0IntegralGain     float32
	FastDie0ProportionalGain float32
	FastDie0PropTgtDelta     uint32
	FastDie0ReleaseTemp      uint32
	FenderIdleOffDelayMS     uint32
	FwEarlyWakeTimeoutMS     uint32
	IdleOffDelayMS           uint32
	IdleOffStandbyTimer      uint32
	PerfBoostCEStep          uint32
	PerfBoostMinUtil         uint32
	PerfFilterDropThreshold  uint32
	PerfFilterTimeConstant   uint32
	PerfFilterTimeConstant2  uint32
	PerfIntegralGain         float32
	PerfIntegralGain2        float32
	PerfIntegralMinClamp     uint32
	PerfProportionalGain     float32
	PerfProportionalGain2    float32
	PerfResetIters           uint32
	PerfTgtUtilization       uint32
	PowerSamplePeriod        uint32
	PPMFilterTimeConstantMS  uint32
	PPMKi, PPMKp             float32
	PwrFilterTimeConstant    uint32
	PwrIntegralGain          float32
	PwrIntegralMinClamp      uint32
	PwrMinDutyCycle          uint32
	PwrProportionalGain      float32
	PwrSamplePeriodAICClks   uint32
	SEEngagementCriteria     int32
	SEFilterTimeConstant     uint32
	SEFilterTimeConstant1    uint32
	SEInactiveThreshold      uint32
	SEKi, SEKi1, SEKp, SEKp1 float32
	SEResetCriteria          uint32
}

const maxPowerZones = 5

// PwrFromFDT reads the GPU node's power configuration.
func PwrFromFDT(n *fdt.Node, hw HwConfig) (PwrConfig, error) {
	var p PwrConfig
	refs, err := n.Refs("operating-points-v2", "")
	if err != nil || len(refs) == 0 {
		return p, fmt.Errorf("agx: %s: no operating-points-v2 (%v)", n.Path, err)
	}
	for _, opp := range refs[0].Node.Children {
		hz := opp.U32s("opp-hz")
		uv := opp.U32s("opp-microvolt")
		uw := opp.U32s("opp-microwatt")
		if len(hz) != 2 || len(uw) != 1 {
			return p, fmt.Errorf("agx: %s: bad operating point", opp.Path)
		}
		if len(uv) != hw.MaxClusters {
			return p, fmt.Errorf("agx: %s: %d voltages, want %d", opp.Path, len(uv), hw.MaxClusters)
		}
		ps := PState{FreqHz: uint32(uint64(hz[0])<<32 | uint64(hz[1])), PwrMW: uw[0] / 1000}
		for _, v := range uv {
			ps.VoltMV = append(ps.VoltMV, v/1000)
		}
		p.PerfStates = append(p.PerfStates, ps)
	}
	if len(p.PerfStates) < 2 {
		return p, fmt.Errorf("agx: %s: %d operating points", n.Path, len(p.PerfStates))
	}

	var missing []string
	u32 := func(name string, def ...uint32) uint32 {
		if v := n.U32s(name); len(v) == 1 {
			return v[0]
		}
		if len(def) > 0 {
			return def[0]
		}
		missing = append(missing, name)
		return 0
	}
	f32 := func(name string, def ...float32) float32 {
		if v := n.U32s(name); len(v) == 1 {
			return math.Float32frombits(v[0]) // m1n1 stores floats as IEEE bits
		}
		if len(def) > 0 {
			return def[0]
		}
		missing = append(missing, name)
		return 0
	}
	floats := func(name string) []float32 {
		var out []float32
		for _, v := range n.U32s(name) {
			out = append(out, math.Float32frombits(v))
		}
		return out
	}

	pz := n.U32s("apple,power-zones")
	if len(pz) > 3*maxPowerZones || len(pz)%3 != 0 {
		return p, fmt.Errorf("agx: bad apple,power-zones")
	}
	for i := 0; i+2 < len(pz); i += 3 {
		p.PowerZones = append(p.PowerZones, PowerZone{pz[i], pz[i+1], pz[i+2]})
	}
	p.CoreLeak, p.SramLeak = floats("apple,core-leak-coef"), floats("apple,sram-leak-coef")
	if len(p.CoreLeak) != hw.MaxClusters || len(p.SramLeak) != hw.MaxClusters {
		return p, fmt.Errorf("agx: leak coefficients: %d core, %d sram, want %d", len(p.CoreLeak), len(p.SramLeak), hw.MaxClusters)
	}

	for _, ps := range p.PerfStates {
		p.MaxPowerMW = max(p.MaxPowerMW, ps.PwrMW)
		p.MaxFreqMHz = max(p.MaxFreqMHz, ps.FreqHz/1_000_000)
	}
	p.PerfBasePState = u32("apple,perf-base-pstate", 1)
	p.PerfMaxPState = uint32(len(p.PerfStates) - 1)
	p.MinSramMicrovolt = u32("apple,min-sram-microvolt")
	p.AvgPowerFilterTCms = u32("apple,avg-power-filter-tc-ms")
	p.AvgPowerKiOnly = f32("apple,avg-power-ki-only")
	p.AvgPowerKp = f32("apple,avg-power-kp")
	p.AvgPowerMinDutyCycle = u32("apple,avg-power-min-duty-cycle")
	p.AvgPowerTargetFilterTC = u32("apple,avg-power-target-filter-tc")
	p.FastDie0IntegralGain = f32("apple,fast-die0-integral-gain")
	p.FastDie0ProportionalGain = f32("apple,fast-die0-proportional-gain")
	p.FastDie0PropTgtDelta = u32("apple,fast-die0-prop-tgt-delta", 0)
	p.FastDie0ReleaseTemp = u32("apple,fast-die0-release-temp", 80)
	p.FenderIdleOffDelayMS = u32("apple,fender-idle-off-delay-ms", 40)
	p.FwEarlyWakeTimeoutMS = u32("apple,fw-early-wake-timeout-ms", 5)
	p.IdleOffDelayMS = u32("apple,idle-off-delay-ms", 2)
	p.IdleOffStandbyTimer = u32("apple,idleoff-standby-timer", hw.IdleOffStandbyTimerDefault)
	p.PerfBoostCEStep = u32("apple,perf-boost-ce-step", 25)
	p.PerfBoostMinUtil = u32("apple,perf-boost-min-util", 100)
	p.PerfFilterDropThreshold = u32("apple,perf-filter-drop-threshold")
	p.PerfFilterTimeConstant2 = u32("apple,perf-filter-time-constant2")
	p.PerfFilterTimeConstant = u32("apple,perf-filter-time-constant")
	p.PerfIntegralGain2 = f32("apple,perf-integral-gain2")
	p.PerfIntegralGain = f32("apple,perf-integral-gain", 7.8956833)
	p.PerfIntegralMinClamp = u32("apple,perf-integral-min-clamp")
	p.PerfProportionalGain2 = f32("apple,perf-proportional-gain2")
	p.PerfProportionalGain = f32("apple,perf-proportional-gain", 14.707963)
	p.PerfResetIters = u32("apple,perf-reset-iters", 6)
	p.PerfTgtUtilization = u32("apple,perf-tgt-utilization")
	p.PowerSamplePeriod = u32("apple,power-sample-period")
	p.PPMFilterTimeConstantMS = u32("apple,ppm-filter-time-constant-ms")
	p.PPMKi = f32("apple,ppm-ki")
	p.PPMKp = f32("apple,ppm-kp")
	p.PwrFilterTimeConstant = u32("apple,pwr-filter-time-constant", 313)
	p.PwrIntegralGain = f32("apple,pwr-integral-gain", 0.0202129)
	p.PwrIntegralMinClamp = u32("apple,pwr-integral-min-clamp", 0)
	p.PwrMinDutyCycle = u32("apple,pwr-min-duty-cycle")
	p.PwrProportionalGain = f32("apple,pwr-proportional-gain", 5.2831855)
	p.PwrSamplePeriodAICClks = u32("apple,pwr-sample-period-aic-clks", uint32(hw.BaseClockHz/1000)*p.PowerSamplePeriod)
	p.SEEngagementCriteria = int32(u32("apple,se-engagement-criteria", math.MaxUint32)) // -1
	p.SEFilterTimeConstant = u32("apple,se-filter-time-constant", 9)
	p.SEFilterTimeConstant1 = u32("apple,se-filter-time-constant-1", 3)
	p.SEInactiveThreshold = u32("apple,se-inactive-threshold", 2500)
	p.SEKi = f32("apple,se-ki", -50)
	p.SEKi1 = f32("apple,se-ki-1", -100)
	p.SEKp = f32("apple,se-kp", -5)
	p.SEKp1 = f32("apple,se-kp-1", -10)
	p.SEResetCriteria = u32("apple,se-reset-criteria", 50)
	if len(missing) > 0 {
		return p, fmt.Errorf("agx: %s lacks %v (m1n1 sets them)", n.Path, missing)
	}
	if p.PowerSamplePeriod == 0 || p.PPMFilterTimeConstantMS < p.PowerSamplePeriod ||
		p.AvgPowerFilterTCms < p.PowerSamplePeriod || p.PerfBasePState >= p.PerfMaxPState {
		return p, fmt.Errorf("agx: inconsistent power configuration (sample period %d ms)", p.PowerSamplePeriod)
	}
	return p, nil
}

// blobRegion finds a calibration blob region ("hw-cal-a", "hw-cal-b", "globals"),
// if m1n1 provided one: what the firmware's own driver built, to compare.
func blobRegion(n *fdt.Node, name string) (Region, bool) {
	regions, err := n.Refs("memory-region", "")
	if err != nil {
		return Region{}, false
	}
	for i, nm := range n.Strings("memory-region-names") {
		if nm == name && i < len(regions) {
			if rr := regions[i].Node.Reg(); len(rr) > 0 && rr[0].Size > 0 {
				size := rr[0].Size
				if s := n.U32s("debug," + name + "-size"); len(s) == 1 {
					size = uint64(s[0])
				}
				return Region{rr[0].Base, size}, true
			}
		}
	}
	return Region{}, false
}
