package agx

import (
	"encoding/binary"
	"math"
	"testing"

	"godeep/drivers/apple/agx/fw"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

func TestInitData(t *testing.T) {
	c, err := ConfigFromFDT(gpuTree(t, true))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Pwr
	if len(p.PerfStates) != 7 || p.MaxFreqMHz != 1296 || p.MaxPowerMW != 166743 || p.PerfMaxPState != 6 ||
		p.PerfStates[1].VoltMV[1] != 634 || p.PPMKi != 30 || p.PwrSamplePeriodAICClks != 24000*8 {
		t.Fatalf("power config %+v", p)
	}
	id, _ := ReadID(sgxFake(0xffff))

	for _, compat := range [][3]uint32{{12, 3, 0}, {13, 5, 0}} {
		L, _ := fw.For(13, compat)
		a := dma.New(dma.Region{Base: 0x108_0000_0000, Mem: make([]byte, 64<<20)})
		phys := mmio.NewFake()
		pages := &DMAPages{A: a}
		kern, _ := BorrowPageTable(phys, pages, 0x100_0000_0000, T6000.OAS, KernBase, KernTop)
		lower, _ := NewPageTable(phys, pages, T6000.OAS, UserBase, UserTop)
		k := NewKernelAlloc(kern, a)
		ch, err := NewChannels(k, compat)
		if err != nil {
			t.Fatal(err)
		}
		d, err := BuildInitData(k, lower, L, T6000, id, p, compat, 0x10_0301_4000, ch)
		if err != nil {
			t.Fatalf("%v: %v", compat, err)
		}

		u32 := func(o *Object, s, f string, i int) uint32 {
			fl, ok := L.Get(s).Field(f)
			if !ok {
				t.Fatalf("%v: %s.%s missing", compat, s, f)
			}
			sz := 4
			return binary.LittleEndian.Uint32(o.Bytes()[fl.Off+i*sz:])
		}
		u64 := func(o *Object, s, f string) uint64 {
			return binary.LittleEndian.Uint64(o.Bytes()[L.Get(s).Off(f):])
		}
		f32 := func(o *Object, s, f string) float32 { return math.Float32frombits(u32(o, s, f, 0)) }

		// The root points at everything, and describes the UAT.
		if u64(d.Root, "initdata.InitData", "runtime_pointers") != d.RuntimePointers.VA ||
			u64(d.Root, "initdata.InitData", "globals") != d.Globals.VA ||
			u64(d.Root, "initdata.InitData", "fw_status") != d.FwStatus.VA {
			t.Fatalf("%v: root pointers", compat)
		}
		lvl := L.Get("initdata.InitData").Off("uat_level_info")
		if b := d.Root.Bytes()[lvl:]; b[3] != 36 || binary.LittleEndian.Uint16(b[4:]) != 8 ||
			binary.LittleEndian.Uint64(b[0x18:]) != 7<<36 || binary.LittleEndian.Uint64(b[0x10:]) != (1<<42-1)&^(PageSize-1) {
			t.Fatalf("%v: UAT level 0 % x", compat, b[:0x20])
		}
		hasVer := L.Get("initdata.InitData").Has("ver_info")
		if hasVer != (compat[0] == 13) || hasVer && binary.LittleEndian.Uint16(d.Root.Bytes()) != 0x6ba0 {
			t.Fatalf("%v: ver_info", compat)
		}

		// RuntimePointers → HwDataA/B and the channels.
		rp := "initdata.RuntimePointers"
		if u64(d.RuntimePointers, rp, "hwdata_a") != d.HwA.VA || u64(d.RuntimePointers, rp, "hwdata_b_2") != d.HwB.VA ||
			u64(d.RuntimePointers, rp, "fwlog_buf") != ch.FwLogPayload.VA {
			t.Fatalf("%v: runtime pointers", compat)
		}
		dc := L.Get(rp).Off("device_control")
		if st, rg := ch.DeviceControl.Raw(); binary.LittleEndian.Uint64(d.RuntimePointers.Bytes()[dc:]) != st ||
			binary.LittleEndian.Uint64(d.RuntimePointers.Bytes()[dc+8:]) != rg {
			t.Fatalf("%v: device control ring", compat)
		}
		pipes := L.Get(rp).Off("pipes")
		if st, _ := ch.Pipes[3][2].Raw(); binary.LittleEndian.Uint64(d.RuntimePointers.Bytes()[pipes+3*0x30+0x20:]) != st {
			t.Fatalf("%v: compute pipe 3", compat)
		}
		if pa, _, ok := lower.Lookup(iovaBufMgrLow); !ok || pa != d.BufMgrCtl.Buf.Phys {
			t.Fatalf("%v: buffer manager not mapped low", compat)
		}
		if pa, _, ok := kern.Lookup(iovaBufMgrHigh); !ok || pa != d.BufMgrCtl.Buf.Phys {
			t.Fatalf("%v: buffer manager not mapped high", compat)
		}

		// HwDataA: power-loop values (Asahi's formulas, t600x numbers).
		ha := "initdata.HwDataA"
		if u32(d.HwA, ha, "max_pstate_scaled", 0) != 600 || u32(d.HwA, ha, "base_pstate_scaled", 0) != 100 ||
			u32(d.HwA, ha, "clocks_per_period", 0) != 192000 || u32(d.HwA, ha, "pwr_status", 0) != 4 ||
			u32(d.HwA, ha, "ppm_filter_tc_periods_x4", 0) != 48 || u32(d.HwA, ha, "max_freq_mhz", 0) != 1296 {
			t.Fatalf("%v: HwDataA scalars", compat)
		}
		if f32(d.HwA, ha, "ppm_ki_dt") != float32(30)*0.008 || f32(d.HwA, ha, "boost_state_unk_k") != float32(5)/0.95 ||
			f32(d.HwA, ha, "pwr_integral_min_clamp") != 0 || f32(d.HwA, ha, "max_power_1") != 166743 {
			t.Fatalf("%v: HwDataA floats", compat)
		}
		if math.Float32frombits(u32(d.HwA, ha, "unk_coef_a1", 8)) != 9.819 || u32(d.HwA, ha, "unk_coef_a1", 16) != 0 {
			t.Fatalf("%v: per-cluster coefficients", compat)
		}
		hws1 := L.Get(ha).Off("hws1") + L.Get("initdata.HwDataShared1").Off("unk_a4")
		if binary.LittleEndian.Uint32(d.HwA.Bytes()[hws1:]) != 0xffff {
			t.Fatalf("%v: hws1", compat)
		}

		// HwDataB: operating points, chip, I/O mappings.
		hb := "initdata.HwDataB"
		if u32(d.HwB, hb, "chip_id", 0) != 0x6000 || u32(d.HwB, hb, "gpu_core_id", 0) != 12 ||
			u32(d.HwB, hb, "gpu_rev_id", 0) != 4 || u32(d.HwB, hb, "num_cores", 0) != 16 ||
			u64(d.HwB, hb, "uat_ttb_base") != 0x10_0301_4000 || u32(d.HwB, hb, "max_pstate", 0) != 6 {
			t.Fatalf("%v: HwDataB scalars", compat)
		}
		if u32(d.HwB, hb, "frequencies", 6) != 1296 || u32(d.HwB, hb, "voltages", 6*8+7) != 903 ||
			u32(d.HwB, hb, "voltages_sram", 1*8) != 790 || u32(d.HwB, hb, "rel_max_powers", 6) != 100 ||
			u32(d.HwB, hb, "rel_boost_freqs", 6) != 100 || u32(d.HwB, hb, "rel_boost_freqs", 1) != 0 {
			t.Fatalf("%v: operating points", compat)
		}
		if u64(d.HwB, hb, "timestamp_area_base") != iovaTimestampBase {
			t.Fatalf("%v: timestamp base %#x", compat, u64(d.HwB, hb, "timestamp_area_base"))
		}
		ae4 := u32(d.HwB, hb, "unk_ae4", 1)
		if compat[0] == 13 && ae4 != 3 || compat[0] == 12 && ae4 != 0xf {
			t.Fatalf("%v: unk_ae4[1] = %#x", compat, ae4)
		}
		// I/O mapping 11 (MCache: 4 controllers 16 MB apart) and 10
		// (GMGIFAF: not page-aligned).
		io := L.Get(hb).Off("io_mappings")
		rec := func(i int) (phys, virt uint64, total, elem uint32) {
			b := d.HwB.Bytes()[io+i*0x20:]
			return binary.LittleEndian.Uint64(b), binary.LittleEndian.Uint64(b[8:]),
				binary.LittleEndian.Uint32(b[16:]), binary.LittleEndian.Uint32(b[20:])
		}
		if ph, va, tot, el := rec(11); ph != 0x200000000 || tot != 4*0xd8000 || el != 0xd8000 || va < mmioLo {
			t.Fatalf("%v: MCache mapping %#x %#x %#x %#x", compat, ph, va, tot, el)
		} else if pa, _, ok := kern.Lookup(va + 3*(0xd8000+PageSize)); !ok || pa != 0x203000000 {
			t.Fatalf("%v: 4th MCache controller at %#x", compat, pa)
		}
		if ph, va, _, _ := rec(10); ph != 0x204d61000 || va&(PageSize-1) != 0x1000 {
			t.Fatalf("%v: GMGIFAF mapping %#x %#x", compat, ph, va)
		} else if pa, pte, _ := kern.Lookup(va); pa != 0x204d61000 || pte>>attrShift&7 != attrDevice {
			t.Fatalf("%v: GMGIFAF maps to %#x (pte %#x)", compat, pa, pte)
		}
		if ph, _, _, _ := rec(4); ph != 0 {
			t.Fatalf("%v: unused mapping 4 filled", compat)
		}

		// Globals.
		g := "initdata.Globals"
		if u32(d.Globals, g, "fault_control", 0) != 0xb || u32(d.Globals, g, "max_power", 0) != 166743 ||
			u32(d.Globals, g, "unk_118e8", 0) != 1 || u32(d.Globals, g, "do_init", 0) != 1 {
			t.Fatalf("%v: Globals", compat)
		}
		tab := L.Get(g).Off("unk_118ec")
		if d.Globals.Bytes()[tab+5] != 90 {
			t.Fatalf("%v: global table", compat)
		}

		// FwStatus points at the fwctl channel.
		if st, _ := ch.FwCtl.Raw(); u64(d.FwStatus, "initdata.FwStatus", "fwctl_channel") != st {
			t.Fatalf("%v: fwctl channel", compat)
		}
	}
}
