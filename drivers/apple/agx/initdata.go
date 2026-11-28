package agx

import (
	"fmt"

	"godeep/drivers/apple/agx/fw"
)

// InitData is the firmware's initialization structure and everything it
// points to (Asahi: initdata.rs InitDataBuilder, gpu.rs iomap). Its
// address goes to the firmware in the MSG_INIT message.
type InitData struct {
	Root, RuntimePointers, Globals, HwA, HwB, FwStatus *Object
	BufMgrCtl                                          *Object
}

// Kernel addresses of fixed things (Asahi: gpu.rs, mmu.rs).
const (
	iovaTimestampBase = 0xffffffae10000000
	iovaBufMgrLow     = 0x20_0000_0000     // in the lower kernel VM (TTB0)
	iovaBufMgrHigh    = 0xffffffaeffff0000 // in the kernel VM (TTB1)
	iovaUnkPage       = UserTop - 2*PageSize
	maxCoresPerClust  = 8   // MAX_CORES_PER_CLUSTER on G13
	faultControl      = 0xb // macOS's default (Asahi module parameter)
)

// initBuilder holds the inputs.
type initBuilder struct {
	L      fw.Layouts
	k      *KernelAlloc
	hw     HwConfig
	id     ID
	pwr    PwrConfig
	compat [3]uint32
	ttbs   uint64 // physical base of the TTBAT ("uat-ttbs")
}

func (b *initBuilder) atLeast(maj, min uint32) bool {
	return b.compat[0] > maj || b.compat[0] == maj && b.compat[1] >= min
}

// v13 is "V >= V13_0B4"; layouts carry the rest of the version logic.
func (b *initBuilder) v13() bool  { return b.compat[0] >= 13 }
func (b *initBuilder) v133() bool { return b.atLeast(13, 3) }

func (b *initBuilder) object(c Class, structName string) (*Object, *fw.Writer, error) {
	o, err := b.k.Alloc(c, b.L.Get(structName).Size)
	if err != nil {
		return nil, nil, err
	}
	return o, b.L.Writer(structName, o.Bytes()), nil
}

// BuildInitData allocates and fills InitData. lower is the lower kernel
// VM (TTB0), where the buffer manager control block is also mapped.
func BuildInitData(k *KernelAlloc, lower *PageTable, L fw.Layouts, hw HwConfig, id ID, pwr PwrConfig,
	compat [3]uint32, ttbsBase uint64, ch *Channels) (*InitData, error) {
	if len(pwr.PerfStates) < 2 || pwr.PowerSamplePeriod == 0 || pwr.PerfBasePState >= pwr.PerfMaxPState {
		return nil, fmt.Errorf("agx: no usable power configuration")
	}
	b := &initBuilder{L: L, k: k, hw: hw, id: id, pwr: pwr, compat: compat, ttbs: ttbsBase}
	d := &InitData{}
	var err error
	if d.HwA, err = b.hwDataA(); err != nil {
		return nil, err
	}
	if d.HwB, err = b.hwDataB(); err != nil {
		return nil, err
	}
	if d.RuntimePointers, d.BufMgrCtl, err = b.runtimePointers(d.HwA, d.HwB, lower, ch); err != nil {
		return nil, err
	}
	if d.Globals, err = b.globals(); err != nil {
		return nil, err
	}
	var fs *fw.Writer
	if d.FwStatus, fs, err = b.object(Shared, "initdata.FwStatus"); err != nil {
		return nil, err
	}
	ring(fs, "fwctl_channel", L, ch.FwCtl.Ring)
	if d.Root, err = b.root(d); err != nil {
		return nil, err
	}
	return d, nil
}

func ring(w *fw.Writer, field string, L fw.Layouts, r *Ring) {
	rw := w.At(field, "initdata.ChannelRing", L)
	st, rg := r.Raw()
	rw.Ptr("state", st)
	rw.Ptr("ring", rg)
}

// hwShared1, hwShared2, hwShared3 fill the structures HwDataA and Globals
// both embed.
func (b *initBuilder) hwShared(w *fw.Writer) {
	h1 := w.At("hws1", "initdata.HwDataShared1", b.L)
	h1.Set("unk_a4", int64(b.hw.Shared1A4))
	for i, v := range b.hw.Shared1Tab {
		h1.SetAt("table", i, int64(v))
	}
	h2 := w.At("hws2", "initdata.HwDataShared2", b.L)
	h2.Fill("unk_28", 0xff)
	h2.Set("unk_508", int64(b.hw.Shared2Unk508))
	for i, v := range b.hw.Shared2Tab {
		h2.SetAt("table", i, int64(v))
	}
	// No shared2 curves and no shared3 table on t600x: both stay zero.
}

func (b *initBuilder) hwDataA() (*Object, error) {
	o, w, err := b.object(Private, "initdata.HwDataA")
	if err != nil {
		return nil, err
	}
	p := b.pwr
	periodMS := p.PowerSamplePeriod
	periodS := float32(periodMS) / 1000
	ppmTCPeriods := p.PPMFilterTimeConstantMS / periodMS
	ppmTCmsRounded := ppmTCPeriods * periodMS
	ppmA := 1 / float32(ppmTCPeriods)
	perfA := 1 / float32(p.PerfFilterTimeConstant)
	perfA2 := 1 / float32(p.PerfFilterTimeConstant2)
	avgTgtA := 1 / float32(p.AvgPowerTargetFilterTC)
	avgTCPeriods := p.AvgPowerFilterTCms / periodMS
	avgTCmsRounded := avgTCPeriods * periodMS
	avgA := 1 / float32(avgTCPeriods)
	pwrA := 1 / float32(p.PwrFilterTimeConstant)
	baseScaled := 100 * p.PerfBasePState
	maxScaled := 100 * p.PerfMaxPState
	boostCount := p.PerfMaxPState - p.PerfBasePState
	baseKHz := uint32(b.hw.BaseClockHz / 1000)
	clocksCoarse := uint32(b.hw.BaseClockHz/1000) * periodMS
	ms := int64(maxScaled)
	maxPwr := int64(p.MaxPowerMW)

	w.Set("clocks_per_period", int64(p.PwrSamplePeriodAICClks))
	w.Set("clocks_per_period_2", int64(p.PwrSamplePeriodAICClks))
	w.Set("pwr_status", 4)
	w.F32("unk_10", 1)
	w.Set("actual_pstate", 1)
	w.Set("tgt_pstate", 1)
	w.Set("base_pstate_scaled", int64(baseScaled))
	w.Set("unk_40", 1)
	w.Set("max_pstate_scaled", ms)
	w.Set("min_pstate_scaled", 100)
	w.Set("unk_64c", 625)
	w.F32("pwr_filter_a_neg", 1-pwrA)
	w.F32("pwr_filter_a", pwrA)
	w.F32("pwr_integral_gain", p.PwrIntegralGain)
	w.Set("pwr_integral_min_clamp", int64(p.PwrIntegralMinClamp))
	w.Set("max_power_1", maxPwr)
	w.F32("pwr_proportional_gain", p.PwrProportionalGain)
	w.F32("pwr_pstate_related_k", -float32(maxScaled)/float32(p.MaxPowerMW))
	w.Set("pwr_pstate_max_dc_offset", int64(int32(p.PwrMinDutyCycle)-int32(maxScaled)))
	w.Set("max_pstate_scaled_2", ms)
	w.Set("max_power_2", maxPwr)
	w.Set("max_pstate_scaled_3", ms)
	w.Set("ppm_filter_tc_periods_x4", int64(ppmTCPeriods*4))
	w.F32("ppm_filter_a_neg", 1-ppmA)
	w.F32("ppm_filter_a", ppmA)
	w.F32("ppm_ki_dt", p.PPMKi*periodS)
	w.F32("unk_6fc", 65536)
	w.F32("ppm_kp", p.PPMKp)
	w.Set("pwr_min_duty_cycle", int64(p.PwrMinDutyCycle))
	w.Set("max_pstate_scaled_4", ms)
	w.F32("unk_71c", 0)
	w.Set("max_power_3", maxPwr)
	w.Set("cur_power_mw_2", 0)
	w.Set("ppm_filter_tc_ms", int64(p.PPMFilterTimeConstantMS))
	w.Set("ppm_filter_tc_clks", int64(ppmTCmsRounded*baseKHz))
	w.Set("perf_tgt_utilization", int64(p.PerfTgtUtilization))
	w.Set("perf_boost_min_util", int64(p.PerfBoostMinUtil))
	w.Set("perf_boost_ce_step", int64(p.PerfBoostCEStep))
	w.Set("perf_reset_iters", int64(p.PerfResetIters))
	w.Set("unk_774", 6)
	w.Set("unk_778", 1)
	w.Set("perf_filter_drop_threshold", int64(p.PerfFilterDropThreshold))
	w.F32("perf_filter_a_neg", 1-perfA)
	w.F32("perf_filter_a2_neg", 1-perfA2)
	w.F32("perf_filter_a", perfA)
	w.F32("perf_filter_a2", perfA2)
	w.F32("perf_ki", p.PerfIntegralGain)
	w.F32("perf_ki2", p.PerfIntegralGain2)
	w.Set("perf_integral_min_clamp", int64(p.PerfIntegralMinClamp))
	w.F32("unk_79c", 95)
	w.F32("perf_kp", p.PerfProportionalGain)
	w.F32("perf_kp2", p.PerfProportionalGain2)
	w.F32("boost_state_unk_k", float32(boostCount)/0.95)
	w.Set("base_pstate_scaled_2", int64(baseScaled))
	w.Set("max_pstate_scaled_5", ms)
	w.Set("base_pstate_scaled_3", int64(baseScaled))
	w.Set("perf_tgt_utilization_2", int64(p.PerfTgtUtilization))
	w.Set("base_pstate_scaled_4", int64(baseScaled))
	w.F32("unk_7fc", 65536)
	w.Set("pwr_min_duty_cycle_2", int64(p.PwrMinDutyCycle))
	w.Set("max_pstate_scaled_6", ms)
	w.Set("max_freq_mhz", int64(p.MaxFreqMHz))
	w.Set("pwr_min_duty_cycle_3", int64(p.PwrMinDutyCycle))
	w.F32("min_pstate_scaled_4", 100)
	w.Set("max_pstate_scaled_7", ms)
	w.F32("unk_alpha_neg", 0.8)
	w.F32("unk_alpha", 0.2)
	w.Set("fast_die0_sensor_mask", int64(b.hw.FastSensorMask[0]))
	w.Set("fast_die0_release_temp_cc", int64(100*p.FastDie0ReleaseTemp))
	w.Set("unk_87c", int64(b.hw.DA87C))
	w.Set("unk_880", 4)
	w.F32("unk_894", 1)
	w.F32("fast_die0_ki_dt", p.FastDie0IntegralGain*periodS)
	w.F32("unk_8a8", 65536)
	w.F32("fast_die0_kp", p.FastDie0ProportionalGain)
	w.Set("pwr_min_duty_cycle_4", int64(p.PwrMinDutyCycle))
	w.Set("max_pstate_scaled_8", ms)
	w.Set("max_pstate_scaled_9", ms)
	w.Set("fast_die0_prop_tgt_delta", int64(100*p.FastDie0PropTgtDelta))
	w.Set("unk_8cc", int64(b.hw.DA8CC))
	w.Set("max_pstate_scaled_10", ms)
	w.Set("max_pstate_scaled_11", ms)
	w.Set("unk_c2c", 1)
	w.Set("power_zone_count", int64(len(p.PowerZones)))
	w.Set("max_power_4", maxPwr)
	w.Set("max_power_5", maxPwr)
	w.Set("max_power_6", maxPwr)
	w.F32("avg_power_target_filter_a_neg", 1-avgTgtA)
	w.F32("avg_power_target_filter_a", avgTgtA)
	w.Set("avg_power_target_filter_tc_x4", int64(4*p.AvgPowerTargetFilterTC))
	w.Set("avg_power_target_filter_tc_xperiod", int64(periodMS*p.AvgPowerTargetFilterTC))
	w.Set("avg_power_target_filter_tc_clks", int64(periodMS*p.AvgPowerTargetFilterTC*baseKHz))
	w.Set("avg_power_filter_tc_periods_x4", int64(4*avgTCPeriods))
	w.F32("avg_power_filter_a_neg", 1-avgA)
	w.F32("avg_power_filter_a", avgA)
	w.F32("avg_power_ki_dt", p.AvgPowerKiOnly*periodS)
	w.F32("unk_d20", 65536)
	w.F32("avg_power_kp", p.AvgPowerKp)
	w.Set("avg_power_min_duty_cycle", int64(p.AvgPowerMinDutyCycle))
	w.Set("max_pstate_scaled_12", ms)
	w.Set("max_pstate_scaled_13", ms)
	w.Set("max_power_7", maxPwr)
	w.Set("max_power_8", maxPwr)
	w.Set("avg_power_filter_tc_ms", int64(p.AvgPowerFilterTCms))
	w.Set("avg_power_filter_tc_clks", int64(avgTCmsRounded*baseKHz))
	w.Set("max_pstate_scaled_14", ms)
	// t81xx_data: nothing on t600x.
	if b.v13() {
		e := w.At("unk_e10_0", "initdata.HwDataA130Extra", b.L)
		fa := 1 / float32(p.SEFilterTimeConstant)
		f1a := 1 / float32(p.SEFilterTimeConstant1)
		e.Set("unk_38", 4)
		e.Set("unk_3c", 8000)
		e.Set("gpu_se_inactive_threshold", int64(p.SEInactiveThreshold))
		e.Set("gpu_se_engagement_criteria", int64(p.SEEngagementCriteria))
		e.Set("gpu_se_reset_criteria", int64(p.SEResetCriteria))
		e.Set("unk_54", 50)
		e.Set("unk_58", 1)
		e.F32("gpu_se_filter_a_neg", 1-fa)
		e.F32("gpu_se_filter_1_a_neg", 1-f1a)
		e.F32("gpu_se_filter_a", fa)
		e.F32("gpu_se_filter_1_a", f1a)
		e.F32("gpu_se_ki_dt", p.SEKi*periodS)
		e.F32("gpu_se_ki_1_dt", p.SEKi1*periodS)
		e.F32("unk_7c", 65536)
		e.F32("gpu_se_kp", p.SEKp)
		e.F32("gpu_se_kp_1", p.SEKp1)
		if b.v133() {
			e.Set("unk_8c", 100)
		} else {
			e.Set("unk_8c", 40)
		}
		e.Set("max_pstate_scaled_1", ms)
		e.F32("unk_9c", 8000)
		e.Set("unk_a0", 1400)
		e.Set("gpu_se_filter_time_constant_ms", int64(p.SEFilterTimeConstant*periodMS))
		e.Set("gpu_se_filter_time_constant_1_ms", int64(p.SEFilterTimeConstant1*periodMS))
		e.Set("gpu_se_filter_time_constant_clks", int64(p.SEFilterTimeConstant*clocksCoarse))
		e.Set("gpu_se_filter_time_constant_1_clks", int64(p.SEFilterTimeConstant1*clocksCoarse))
		e.F32("unk_c4", 65536)
		e.F32("unk_114", 65536)
		e.Set("unk_124", 40)
		e.Set("max_pstate_scaled_2", ms)
	}
	w.Set("fast_die0_sensor_mask_2", int64(b.hw.FastSensorMask[0]))
	w.Set("unk_e24", int64(b.hw.DAE24))
	w.Set("unk_e28", 1)
	w.Set("fast_die0_sensor_mask_alt", int64(b.hw.FastSensorMaskAlt[0]))
	w.Set("fast_die0_sensor_present", int64(b.hw.FastDie0SensorPresent)) // < 13.0b4 only
	w.Set("unk_163c", 1)
	w.Set("unk_3644", 0)
	b.hwShared(w)
	w.Set("unk_3ce8", 1)

	for i := range p.PerfStates {
		w.F32At("sram_k", i, b.hw.SramK)
	}
	for i, c := range p.CoreLeak {
		w.F32At("core_leak_coef", i, c)
	}
	for i, c := range p.SramLeak {
		w.F32At("sram_leak_coef", i, c)
	}
	for i := 0; i < b.id.Clusters; i++ {
		if i < len(b.hw.CoefA) {
			w.F32At("unk_coef_a1", i*maxCoresPerClust, b.hw.CoefA[i])
			w.F32At("unk_coef_a2", i*maxCoresPerClust, b.hw.CoefA[i])
		}
		if i < len(b.hw.CoefB) {
			w.F32At("unk_coef_b1", i*maxCoresPerClust, b.hw.CoefB[i])
			w.F32At("unk_coef_b2", i*maxCoresPerClust, b.hw.CoefB[i])
		}
	}
	for i, pz := range p.PowerZones {
		z := w.AtIndex("power_zones", i, "initdata.PowerZone", b.L)
		fa := 1 / float32(pz.FilterTC)
		z.Set("target", int64(pz.Target))
		z.Set("target_off", int64(pz.Target-pz.TargetOffset))
		z.Set("filter_tc_x4", int64(4*pz.FilterTC))
		z.Set("filter_tc_xperiod", int64(periodMS*pz.FilterTC))
		z.F32("filter_a", fa)
		z.F32("filter_a_neg", 1-fa)
		if b.v13() {
			z.Set("unk_10", 1320000000)
		}
	}
	return o, nil
}

func (b *initBuilder) hwDataB() (*Object, error) {
	o, w, err := b.object(Private, "initdata.HwDataB")
	if err != nil {
		return nil, err
	}
	p := b.pwr
	w.Set("unk_0", 0x13_00000000) // < 13.0b4
	w.Set("unk_8", 0x14_00000000)
	w.Set("unk_10", 0x1_00000000) // < 13.0b4
	w.Set("unk_18", 0xffc00000)
	w.Set("unk_30", iovaUnkPage)
	w.Set("timestamp_area_base", -0x51f0000000) // 0xffffffae10000000
	w.Set("chip_id", int64(b.hw.ChipID))
	w.Set("unk_454", int64(b.hw.DB454))
	for _, f := range []string{"unk_458", "unk_460", "unk_464", "unk_468", "unk_47c", "unk_484", "unk_48c",
		"unk_49c", "unk_4a0", "unk_4a4", "unk_4f0", "unk_4f4", "unk_524", "unk_554", "unk_b10", "unk_b24", "unk_b28", "unk_b2c"} {
		w.Set(f, 1)
	}
	w.Set("base_clock_khz", int64(b.hw.BaseClockHz/1000))
	w.Set("power_sample_period", int64(p.PowerSamplePeriod))
	w.Set("unk_4c0", 0x1f)
	w.Set("unk_4e0", int64(b.hw.DB4E0))
	w.Set("unk_504", 0x31)
	w.Set("unk_534", int64(b.hw.DB534))
	w.Set("num_frags", int64(b.id.Cores*b.id.Clusters)) // num_frags = num_cores on G13
	w.Set("uat_ttb_base", int64(b.ttbs))
	w.Set("gpu_core_id", int64(b.hw.CoreID))
	w.Set("gpu_rev_id", int64(revisionID(b.id.Rev)))
	w.Set("num_cores", int64(b.id.Cores*b.id.Clusters))
	w.Set("max_pstate", int64(len(p.PerfStates)-1))
	if !b.v13() {
		w.Set("num_pstates", int64(len(p.PerfStates)))
		w.Set("min_sram_volt", int64(p.MinSramMicrovolt/1000))
		w.Set("unk_ab8", int64(b.hw.DBAB8))
		w.Set("unk_abc", int64(b.hw.DBABC))
		w.Set("unk_ac0", 0x1020)
	}
	ae4 := []int64{0x0, 0xf, 0x3f, 0x3f}
	if b.v13() {
		ae4 = []int64{0x0, 0x3, 0x7, 0x7}
	}
	for i, v := range ae4 {
		w.SetAt("unk_ae4", i, v)
	}
	w.Set("timer_offset", 0)
	w.Set("unk_b30", int64(b.hw.DBB30))
	w.Set("unk_b38_0", 1) // 13.0b4+
	w.Set("unk_b38_4", 1)
	w.Fill("unk_b38", 0xffffffff)
	if b.v133() {
		w.Set("unk_c3c", 0x1a)
		for i := range 16 {
			w.SetAt("unk_arr_0", i, int64(i))
		}
	} else if b.v13() {
		w.Set("unk_c3c", 0x19)
	}

	basePS, maxPS := p.PerfBasePState, p.PerfMaxPState
	baseF, maxF := p.PerfStates[basePS].FreqHz, p.PerfStates[maxPS].FreqHz
	minSram := p.MinSramMicrovolt / 1000
	vcols := w.Len("voltages") / w.Len("frequencies")
	for i, ps := range p.PerfStates {
		w.SetAt("frequencies", i, int64(ps.FreqHz/1_000_000))
		for j := 0; j < vcols; j++ {
			mv := ps.VoltMV[0]
			if j < len(ps.VoltMV) {
				mv = ps.VoltMV[j]
			}
			w.SetAt("voltages", i*vcols+j, int64(mv))
			w.SetAt("voltages_sram", i*vcols+j, int64(max(mv, minSram)))
		}
		w.F32At("sram_k", i, b.hw.SramK)
		w.SetAt("rel_max_powers", i, int64(ps.PwrMW*100/p.MaxPowerMW))
		if uint32(i) > basePS {
			w.SetAt("rel_boost_freqs", i, int64((ps.FreqHz-baseF)/((maxF-baseF)/100)))
		}
	}

	// The register ranges the firmware drives itself.
	f, _ := b.L.Get("initdata.HwDataB").Field("io_mappings")
	slots := f.Size / b.L.Get("initdata.IOMapping").Size
	for idx, m := range b.hw.IOMaps {
		if m == nil {
			continue
		}
		if idx >= slots {
			return nil, fmt.Errorf("agx: I/O mapping %d beyond the firmware's %d", idx, slots)
		}
		if err := b.iomap(w.AtIndex("io_mappings", idx, "initdata.IOMapping", b.L), m); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// iomap maps a register range for the firmware and records it (Asahi:
// GpuManager::iomap).
func (b *initBuilder) iomap(rec *fw.Writer, m *IOMap) error {
	dies := 1
	if m.PerDie {
		dies = b.hw.NumDies
	}
	off := m.Base & (PageSize - 1)
	base := m.Base - off
	end := (m.Base + m.Size + PageSize - 1) &^ (PageSize - 1)
	mapSize := end - base
	if !(off == 0 && mapSize == m.Size || m.Count == 1 && !m.PerDie) || m.Count == 0 {
		return fmt.Errorf("agx: I/O mapping %#x isn't aligned", m.Base)
	}
	var first uint64
	for die := 0; die < dies; die++ {
		for i := 0; i < m.Count; i++ {
			va, err := b.k.MapMMIO(base+uint64(die)*0x20_0000_0000+uint64(i)*m.Stride, mapSize, m.Writable)
			if err != nil {
				return err
			}
			if die == 0 && i == 0 {
				first = va
			} else if va != first+uint64(die*m.Count+i)*(mapSize+PageSize) {
				return fmt.Errorf("agx: I/O mapping %#x isn't contiguous", m.Base)
			}
		}
	}
	rec.Set("phys_addr", int64(m.Base))
	rec.Set("virt_addr", int64(first+off))
	rec.Set("total_size", int64(m.Size)*int64(m.Count*dies))
	rec.Set("element_size", int64(m.Size))
	if m.Writable {
		rec.Set("readwrite", 1)
	}
	return nil
}

// revisionID is GpuRevisionID: A0 = 1 … C1 = 6.
func revisionID(rev string) int {
	for i, r := range []string{"A0", "A1", "B0", "B1", "C0", "C1"} {
		if r == rev {
			return i + 1
		}
	}
	return 0
}

func (b *initBuilder) runtimePointers(hwa, hwb *Object, lower *PageTable, ch *Channels) (rp, bufMgr *Object, err error) {
	alloc := func(n int) *Object {
		if err != nil {
			return nil
		}
		var o *Object
		o, err = b.k.Alloc(Private, n)
		return o
	}
	vtx := alloc(b.L.Get("initdata.GpuGlobalStatsVtx").Size)
	frag := alloc(b.L.Get("initdata.GpuGlobalStatsFrag").Size)
	comp := alloc(b.L.Get("initdata.GpuStatsComp").Size)
	u190, u198 := alloc(0x80), alloc(0xc0)
	u1b8, u1c0, u1c8 := alloc(0x1000), alloc(0x300), alloc(0x1000)
	if err != nil {
		return nil, nil, err
	}
	// The buffer manager control block: GPU-visible low, firmware high.
	if bufMgr, err = b.k.Alloc(GPUShared, 0x4000); err != nil {
		return nil, nil, err
	}
	if err := MapAt(lower, bufMgr, iovaBufMgrLow, ProtGPUSharedRW); err != nil {
		return nil, nil, err
	}
	if err := MapAt(b.k.pt, bufMgr, iovaBufMgrHigh, ProtFWSharedRW); err != nil {
		return nil, nil, err
	}

	o, w, err := b.object(Private, "initdata.RuntimePointers")
	if err != nil {
		return nil, nil, err
	}
	for i := range ch.Pipes {
		pc := w.AtIndex("pipes", i, "initdata.PipeChannels", b.L)
		ring(pc, "vtx", b.L, ch.Pipes[i][0].Ring)
		ring(pc, "frag", b.L, ch.Pipes[i][1].Ring)
		ring(pc, "comp", b.L, ch.Pipes[i][2].Ring)
	}
	ring(w, "device_control", b.L, ch.DeviceControl.Ring)
	ring(w, "event", b.L, ch.Event.Ring)
	ring(w, "fw_log", b.L, ch.FwLog.Ring)
	ring(w, "ktrace", b.L, ch.KTrace.Ring)
	ring(w, "stats", b.L, ch.Stats.Ring)
	w.Ptr("stats_vtx", vtx.VA)
	w.Ptr("stats_frag", frag.VA)
	w.Ptr("stats_comp", comp.VA)
	w.Ptr("hwdata_a", hwa.VA)
	w.Ptr("unkptr_190", u190.VA)
	w.Ptr("unkptr_198", u198.VA)
	w.Ptr("hwdata_b", hwb.VA)
	w.Ptr("hwdata_b_2", hwb.VA)
	w.Ptr("fwlog_buf", ch.FwLogPayload.VA)
	w.Ptr("unkptr_1b8", u1b8.VA)
	w.Ptr("unkptr_1c0", u1c0.VA)
	w.Ptr("unkptr_1c8", u1c8.VA)
	w.Set("buffer_mgr_ctl_gpu_addr", iovaBufMgrLow)
	w.Set("buffer_mgr_ctl_fw_addr", -0x5100010000) // 0xffffffaeffff0000
	w.At("gpu_scratch", "initdata.RuntimeScratch", b.L).Set("unk_6b38", 0xff)
	return o, bufMgr, nil
}

func (b *initBuilder) globals() (*Object, error) {
	o, w, err := b.object(Private, "initdata.Globals")
	if err != nil {
		return nil, err
	}
	p := b.pwr
	periodMS := p.PowerSamplePeriod
	periodS := float32(periodMS) / 1000
	ms := int64(100 * p.PerfMaxPState)

	w.Set("ktrace_enable", 0)
	w.Set("unk_24_0", 3000) // 13.2+
	w.Set("unk_24", 0)
	w.Set("debug", 0)
	w.Set("unk_28", 1)
	w.Set("unk_2c_0", 0) // 13.0b4+ on G13
	w.Set("unk_2c", 1)
	w.Set("unk_30", 0)
	w.Set("unk_34", 120)
	w.Set("unk_54", int64(b.hw.GlobalUnk54))
	w.Set("unk_56", 40)
	w.Set("unk_58", 0xffff)
	w.Set("unk_5e", 1)
	w.Set("unk_66", 1)
	w.Set("unk_8900", 1)
	w.Set("pending_submissions", 0)
	w.Set("max_power", int64(p.MaxPowerMW))
	w.Set("max_pstate_scaled", ms)
	w.Set("max_pstate_scaled_2", ms)
	w.Set("max_pstate_scaled_3", ms)
	w.Set("power_zone_count", int64(len(p.PowerZones)))
	w.Set("avg_power_filter_tc_periods", int64(p.AvgPowerFilterTCms/periodMS))
	w.F32("avg_power_ki_dt", p.AvgPowerKiOnly*periodS)
	w.F32("avg_power_kp", p.AvgPowerKp)
	w.Set("avg_power_min_duty_cycle", int64(p.AvgPowerMinDutyCycle))
	w.Set("avg_power_target_filter_tc", int64(p.AvgPowerTargetFilterTC))
	w.Set("unk_89bc", int64(b.hw.DA8CC))
	w.Set("fast_die0_release_temp", int64(100*p.FastDie0ReleaseTemp))
	w.Set("unk_89c4", int64(b.hw.DA87C))
	w.Set("fast_die0_prop_tgt_delta", int64(100*p.FastDie0PropTgtDelta))
	w.F32("fast_die0_kp", p.FastDie0ProportionalGain)
	w.F32("fast_die0_ki_dt", p.FastDie0IntegralGain*periodS)
	w.Set("unk_89e0", 1)
	w.Set("max_power_2", int64(p.MaxPowerMW))
	w.F32("ppm_kp", p.PPMKp)
	w.F32("ppm_ki_dt", p.PPMKi*periodS)
	w.Set("unk_89f4_8", 1)
	w.Set("unk_89f4", 0)
	b.hwShared(w)
	w.Set("idle_off_standby_timer", int64(p.IdleOffStandbyTimer))
	w.Set("unk_hws2_24", 0)
	w.Set("unk_900c", 1)
	w.Set("unk_9010_0", 1)
	if b.v13() {
		w.Set("unk_903c", 1)
	} else {
		w.Set("unk_903c", 0)
	}
	w.Set("fault_control", faultControl)
	w.Set("do_init", 1)
	w.Set("progress_check_interval_3d", 40)
	w.Set("progress_check_interval_ta", 10)
	w.Set("progress_check_interval_cl", 250)
	w.Set("unk_1102c_0", 1)
	w.Set("unk_1102c_4", 1)
	w.Set("unk_1102c_8", 100)
	w.Set("unk_1102c_c", 1)
	w.Set("idle_off_delay_ms", int64(p.IdleOffDelayMS))
	w.Set("fender_idle_off_delay_ms", int64(p.FenderIdleOffDelayMS))
	w.Set("fw_early_wake_timeout_ms", int64(p.FwEarlyWakeTimeoutMS))
	w.Set("cl_context_switch_timeout_ms", 40)
	w.Set("cl_kill_timeout_ms", 50)
	w.Set("unk_11edc", 0)
	w.Set("unk_11efc", 0)
	for i, pz := range p.PowerZones {
		z := w.AtIndex("power_zones", i, "initdata.PowerZoneGlobal", b.L)
		z.Set("target", int64(pz.Target))
		z.Set("target_off", int64(pz.Target-pz.TargetOffset))
		z.Set("filter_tc", int64(pz.FilterTC))
	}
	if len(b.hw.GlobalTab) > 0 {
		for i, x := range b.hw.GlobalTab {
			w.SetAt("unk_118ec", i, int64(x))
		}
		w.Set("unk_118e8", 1)
	}
	return o, nil
}

func (b *initBuilder) root(d *InitData) (*Object, error) {
	unkBuf, err := b.k.Alloc(SharedRO, 0x4000)
	if err != nil {
		return nil, err
	}
	o, w, err := b.object(Private, "initdata.InitData")
	if err != nil {
		return nil, err
	}
	if b.compat == [3]uint32{13, 5, 0} { // G13
		for i, v := range []int64{0x6ba0, 0x1f28, 0x601, 0xb0} {
			w.SetAt("ver_info", i, v)
		}
	}
	w.Ptr("unk_buf", unkBuf.VA)
	w.Ptr("runtime_pointers", d.RuntimePointers.VA)
	w.Ptr("globals", d.Globals.VA)
	w.Ptr("fw_status", d.FwStatus.VA)
	w.Set("uat_page_size", PageSize)
	w.Set("uat_page_bits", PageBits)
	w.Set("uat_num_levels", 3)
	for i, lv := range []struct{ shift, n uint }{{36, 8}, {25, 2048}, {14, 2048}} {
		u := w.AtIndex("uat_level_info", i, "initdata.UatLevelInfo", b.L)
		u.Set("index_shift", int64(lv.shift))
		u.Set("unk_1", 14)
		u.Set("unk_2", 14)
		u.Set("unk_3", 8)
		u.Set("unk_4", 0x4000)
		u.Set("num_entries", int64(lv.n))
		u.Set("unk_8", 1)
		u.Set("unk_10", int64((uint64(1)<<b.hw.OAS-1)&^(PageSize-1)))
		u.Set("index_mask", int64(uint64(lv.n-1)<<lv.shift))
	}
	w.Set("host_mapped_fw_allocations", 1)
	return o, nil
}
