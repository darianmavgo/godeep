package agx

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"godeep/drivers/apple/agx/fw"
	"godeep/drivers/apple/asc"
	"godeep/drivers/apple/asc/asctest"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/fdt"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
	"godeep/kernel/rtkit"
)

// sgxFake is an M1 Pro's GPU ID registers: G13S, 2 clusters × 8 cores
// with the given core masks.
func sgxFake(mask0 uint32) *mmio.Fake {
	f := mmio.NewFake()
	f.Set(ID_VERSION, 4<<24|3<<16|0x11<<8)
	f.Set(ID_COUNTS_1, 8)
	f.Set(ID_COUNTS_2, 4<<16)
	f.Set(ID_CLUSTERS, 2<<12)
	f.Set(CORE_MASK_0, mask0)
	return f
}

func TestReadID(t *testing.T) {
	id, err := ReadID(sgxFake(0xffff))
	if err != nil {
		t.Fatal(err)
	}
	if got := id.String(); got != "G13S B1: 2 clusters × 8 cores, 16 active (masks 0xff 0xff)" {
		t.Fatalf("ID %q", got)
	}
	if err := T6000.Check(id); err != nil {
		t.Fatal(err)
	}
	// The 14-core M1 Pro: one core fused off per cluster.
	id, err = ReadID(sgxFake(0x7f7f))
	if err != nil || id.ActiveCores != 14 || id.CoreMasks[1] != 0x7f {
		t.Fatalf("binned: %v %v", id, err)
	}
	// Bits beyond the last cluster mean the decoding is wrong.
	f := sgxFake(0xffff)
	f.Set(CORE_MASK_1, 1)
	if _, err := ReadID(f); err == nil {
		t.Fatal("leftover mask bits accepted")
	}
	f = sgxFake(0xffff)
	f.Set(ID_VERSION, 9<<24)
	if _, err := ReadID(f); err == nil {
		t.Fatal("unknown generation accepted")
	}
	if _, err := ReadID(mmio.NewFake()); err == nil {
		t.Fatal("an unpowered GPU (ID 0) accepted")
	}
	// A G13C (M1 Max) isn't this chip.
	f = sgxFake(0xffff)
	f.Set(ID_VERSION, 4<<24|4<<16|0x11<<8)
	id, _ = ReadID(f)
	if T6000.Check(id) == nil {
		t.Fatal("G13C passed the T6000 check")
	}
}

func TestDecodeFault(t *testing.T) {
	if _, ok := DecodeFault(0); ok {
		t.Fatal("no fault decoded as one")
	}
	addr := uint64(0xffffffa000004000) & (1<<IAS - 1)
	info := addr>>6<<30 | 3<<17 | 0xa2<<9 | 1<<4 | 1<<1 | 1
	f, ok := DecodeFault(info)
	if !ok || f.Addr != addr || f.VMSlot != 3 || f.Unit != "dCDM_KS0" || !f.Read || f.Reason != "access flag" {
		t.Fatalf("%+v", f)
	}
	if f, _ := DecodeFault(0x1e<<9 | 1); f.Unit != "ZLS1" {
		t.Fatalf("unit %s", f.Unit)
	}
}

// fakePages hands out page-table pages from a fake physical range.
type fakePages struct{ next, n int }

func (p *fakePages) AllocPage() (uint64, error) {
	p.n++
	p.next++
	return 0x100_0000_0000 + uint64(p.next)*PageSize, nil
}
func (p *fakePages) FreePage(uint64) { p.n-- }

func TestPageTable(t *testing.T) {
	phys := mmio.NewFake()
	pages := &fakePages{}
	pt, err := NewPageTable(phys, pages, T6000.OAS, UserBase, UserTop)
	if err != nil {
		t.Fatal(err)
	}
	pa := uint64(0x108_0000_0000)
	if err := pt.Map(0x1000_0000, pa, 3*PageSize, ProtGPUSharedRW); err != nil {
		t.Fatal(err)
	}
	if pages.n != 3 { // top table + one each of the two lower levels
		t.Fatalf("%d table pages", pages.n)
	}
	got, pte, ok := pt.Lookup(0x1000_0000 + 2*PageSize + 0x123)
	if !ok || got != pa+2*PageSize+0x123 {
		t.Fatalf("lookup %#x %v", got, ok)
	}
	// GPU-only RW, uncached, AF, non-global (a user table), type 3.
	if want := pa + 2*PageSize | 2<<6 | 2<<2 | 1<<10 | 1<<11 | 3 | uint64(highGPUAccess|highUXN)<<52; pte != want {
		t.Fatalf("pte %#x, want %#x", pte, want)
	}
	if err := pt.Map(0x1000_0000+PageSize, pa, PageSize, ProtGPUSharedRW); err == nil {
		t.Fatal("a mapped page was replaced")
	}
	if err := pt.Map(0x1000_0000, pa+1, PageSize, ProtGPUSharedRW); err == nil {
		t.Fatal("unaligned physical address accepted")
	}
	if err := pt.Map(KernBase, pa, PageSize, ProtGPUSharedRW); err == nil {
		t.Fatal("a kernel address mapped in a user table")
	}
	if err := pt.Map(0x1000_0000, 1<<42, PageSize, ProtGPUSharedRW); err == nil {
		t.Fatal("an address beyond the 42-bit OAS accepted")
	}
	if err := pt.Unmap(0x1000_0000, 3*PageSize); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := pt.Lookup(0x1000_0000); ok {
		t.Fatal("still mapped after Unmap")
	}
	// Mapping across a level-1 boundary allocates another leaf table.
	edge := uint64(1)<<(PageBits+lvBits) - PageSize
	if err := pt.Map(edge, pa, 2*PageSize, ProtGPUSharedRW); err != nil || pages.n != 5 { // two new leaf tables
		t.Fatalf("across a boundary: %v, %d pages", err, pages.n)
	}

	// The kernel table: iBoot's TTB1, global entries, top-of-space VAs.
	kern, err := BorrowPageTable(phys, pages, 0x100_0000_0000, T6000.OAS, KernBase, KernTop)
	if err != nil {
		t.Fatal(err)
	}
	if err := kern.Map(KernBase+0x10000, pa, PageSize, ProtFWSharedRW); err != nil {
		t.Fatal(err)
	}
	_, pte, ok = kern.Lookup(KernBase + 0x10000)
	if !ok || pte&pteNonGlobal != 0 || pte != pa|0x448|0xc<<52|3 {
		t.Fatalf("kernel pte %#x", pte)
	}
	if _, err := BorrowPageTable(phys, pages, 0x100_0000_1000, T6000.OAS, KernBase, KernTop); err == nil {
		t.Fatal("unaligned TTB accepted")
	}
}

// firmware plays the GPU firmware's side of the handoff: Peterson's lock
// with the roles swapped, the magic, and an inCritical check.
type firmware struct {
	m          *mmio.Fake
	inCritical atomic.Int32
	violations atomic.Int32
	answer     bool // write the magic
}

func (fw *firmware) lock() {
	fw.m.Write8(hoLockFW, 1)
	for fw.m.Read8(hoLockAP) != 0 {
		if fw.m.Read32(hoTurn) == 0 {
			fw.m.Write8(hoLockFW, 0)
			for fw.m.Read32(hoTurn) == 0 {
			}
			fw.m.Write8(hoLockFW, 1)
		}
	}
}

func (fw *firmware) unlock() {
	fw.m.Write32(hoTurn, 0)
	fw.m.Write8(hoLockFW, 0)
}

func (fw *firmware) critical() {
	if fw.inCritical.Add(1) != 1 {
		fw.violations.Add(1)
	}
	time.Sleep(10 * time.Microsecond)
	fw.inCritical.Add(-1)
}

func (fw *firmware) run(ctx context.Context) {
	for ctx.Err() == nil {
		fw.lock()
		fw.critical()
		if fw.answer && fw.m.Read64(hoMagicAP) == PPLMagic {
			fw.m.Write64(hoMagicFW, PPLMagic)
		}
		fw.unlock()
		time.Sleep(50 * time.Microsecond)
	}
}

func TestHandoff(t *testing.T) {
	m := mmio.NewFake()
	fw := &firmware{m: m, answer: true}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); fw.run(ctx) }()
	h := NewHandoff(m)
	if err := h.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if err := h.Lock(ctx); err != nil {
			t.Fatal(err)
		}
		fw.critical()
		h.Unlock()
	}
	cancel()
	wg.Wait()
	if v := fw.violations.Load(); v != 0 {
		t.Fatalf("both sides in the critical section %d times", v)
	}

	// A firmware that never answers: Init gives up.
	m = mmio.NewFake()
	fw = &firmware{m: m}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go fw.run(ctx)
	start := time.Now()
	if err := NewHandoff(m).Init(ctx); !errors.Is(err, ErrNoFirmware) {
		t.Fatalf("no firmware: %v", err)
	}
	if time.Since(start) < HandoffTimeout {
		t.Fatal("gave up early")
	}
}

func TestTTBs(t *testing.T) {
	m, r := mmio.NewFake(), mmio.NewFake()
	for i := uint64(0); i < NumCtx*slotSize; i += 8 {
		r.Write64(i, 0xdead0000)
	}
	tt := NewTTBs(r)
	if err := tt.SetKernel(context.Background(), NewHandoff(m), 0x1000_0000_4000, 0x1000_0000_8000); err != nil {
		t.Fatal(err)
	}
	if a, b := tt.Slot(0); a != 0x1000_0000_4001 || b != 0x1000_0000_8001 {
		t.Fatalf("slot 0: %#x %#x", a, b)
	}
	if a, b := tt.Slot(NumCtx - 1); a != 0 || b != 0 {
		t.Fatal("other slots not cleared")
	}
	if m.Read8(hoLockAP) != 0 || m.Read32(hoTurn) != 1 {
		t.Fatal("handoff lock not released to the firmware")
	}
}

func TestWrite8KeepsNeighbours(t *testing.T) {
	m := mmio.NewFake()
	m.Write32(0x10, 0xaabbccdd)
	m.Write8(0x11, 0x11)
	if v := m.Read32(0x10); v != 0xaabb11dd {
		t.Fatalf("%#x", v)
	}
}

// gpuTree is the M1 Pro's GPU as Asahi's t600x-die0.dtsi describes it,
// with the regions m1n1 fills in.
func gpuTree(t *testing.T, regions bool) *fdt.Node {
	t.Helper()
	var b fdt.Builder
	b.Begin("")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Begin("reserved-memory")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	for i, r := range []struct {
		name string
		base uint64
		size uint64
	}{{"uat-handoff@10", 0x10_0300_0000, 0x4000}, {"uat-pagetables@20", 0x10_0300_4000, 0x10000}, {"uat-ttbs@30", 0x10_0301_4000, 0x4000}} {
		b.Begin(r.name)
		b.PropU32("phandle", uint32(10+i))
		if regions {
			b.PropRegs(2, 2, fdt.Range{Base: r.base, Size: r.size})
			b.PropStrings("status", "okay")
		} else {
			b.PropStrings("status", "disabled")
		}
		b.End()
	}
	b.End()
	b.Begin("power-management@28e080000")
	b.PropRegs(2, 2, fdt.Range{Base: 0x2_8e08_0000, Size: 0x4000})
	b.PropU32("#address-cells", 1)
	b.PropU32("#size-cells", 1)
	b.Begin("power-controller@4b8")
	b.PropStrings("compatible", "apple,t6000-pmgr-pwrstate", "apple,pmgr-pwrstate")
	b.PropU32("reg", 0x4b8, 8)
	b.PropU32("#power-domain-cells", 0)
	b.PropU32("phandle", 1)
	b.End()
	b.End()
	b.Begin("opp-table-gpu")
	b.PropU32("phandle", 20)
	for i, o := range m1ProOPPs {
		b.Begin(fmt.Sprintf("opp%02d", i))
		b.PropU32("opp-hz", 0, o[0])
		b.PropU32("opp-microvolt", o[1], o[1])
		b.PropU32("opp-microwatt", o[2])
		b.End()
	}
	b.End()
	b.Begin("mbox@406408000")
	b.PropRegs(2, 2, fdt.Range{Base: 0x4_0640_8000, Size: 0x4000})
	b.PropU32("#mbox-cells", 0)
	b.PropU32("phandle", 2)
	b.End()
	b.Begin("gpu@406400000")
	b.PropStrings("compatible", Compatible, "apple,agx-g13x")
	b.PropRegs(2, 2, fdt.Range{Base: 0x4_0640_0000, Size: 0x40000}, fdt.Range{Base: 0x4_0400_0000, Size: 0x1000000})
	b.PropStrings("reg-names", "asc", "sgx")
	b.PropU32("mboxes", 2)
	b.PropU32("power-domains", 1)
	b.PropU32("memory-region", 12, 11, 10)
	b.PropStrings("memory-region-names", "ttbs", "pagetables", "handoff")
	b.PropU32("apple,firmware-version", 12, 3, 0)
	b.PropU32("apple,firmware-compat", 12, 3, 0)
	b.PropU32("operating-points-v2", 20)
	for name, v := range m1ProPower {
		b.PropU32(name, v)
	}
	b.PropU32("apple,core-leak-coef", math.Float32bits(1200), math.Float32bits(1200))
	b.PropU32("apple,sram-leak-coef", math.Float32bits(20), math.Float32bits(20))
	b.End()
	b.End()
	blob, err := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	root, err := fdt.Parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	return root.Find("/gpu@406400000")
}

func TestConfigFromFDT(t *testing.T) {
	c, err := ConfigFromFDT(gpuTree(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if c.ASC != 0x4_0640_0000 || c.SGX != 0x4_0400_0000 || c.Mailbox != 0x4_0640_8000 {
		t.Fatalf("windows %+v", c)
	}
	if c.Handoff.Base != 0x10_0300_0000 || c.PageTables.Base != 0x10_0300_4000 || c.TTBs.Base != 0x10_0301_4000 {
		t.Fatalf("regions %+v %+v %+v", c.Handoff, c.PageTables, c.TTBs)
	}
	if len(c.Power) != 1 || c.Power[0].Addr != 0x2_8e08_04b8 {
		t.Fatalf("power %+v", c.Power)
	}
	if c.Compat != [3]uint32{12, 3, 0} || !SupportedFirmware(c.Compat) {
		t.Fatalf("compat %v", c.Compat)
	}
	// A tree m1n1 didn't fill in (or a loader that isn't m1n1).
	if _, err := ConfigFromFDT(gpuTree(t, false)); err == nil || !strings.Contains(err.Error(), "handoff") {
		t.Fatalf("missing regions: %v", err)
	}
}

// gpuMem is the test's view of GPU kernel memory: it resolves a kernel
// VA through the page tables to the DMA region, as the firmware would.
type gpuMem struct {
	t    *testing.T
	pt   *PageTable
	base uint64
	mem  []byte
}

func (g gpuMem) at(va uint64, n int) []byte {
	pa, _, ok := g.pt.Lookup(va)
	if !ok || pa < g.base || pa-g.base+uint64(n) > uint64(len(g.mem)) {
		g.t.Fatalf("firmware: %#x not mapped to the DMA region (pa %#x)", va, pa)
	}
	return g.mem[pa-g.base : pa-g.base+uint64(n)]
}

func TestDriver(t *testing.T) {
	const memBase = 0x108_0000_0000
	c, err := ConfigFromFDT(gpuTree(t, true))
	if err != nil {
		t.Fatal(err)
	}
	L, _ := fw.For(13, c.Compat)
	sgx, ascR, ho, ttbs, phys := sgxFake(0xffff), mmio.NewFake(), mmio.NewFake(), mmio.NewFake(), mmio.NewFake()
	mem := make([]byte, 64<<20)
	fwh := &firmware{m: ho, answer: true}
	fwCtx, stopFW := context.WithCancel(context.Background())
	defer stopFW()
	var started atomic.Bool
	var initVA atomic.Uint64
	var devctl atomic.Uint32 // the first device-control command, as the firmware sees it
	var model *asctest.Model
	gm := gpuMem{t: t, base: memBase, mem: mem}
	gm.pt, _ = BorrowPageTable(phys, nil, c.PageTables.Base, T6000.OAS, KernBase, KernTop)

	// The firmware: RTKit handshake (endpoints 0x20 and 0x21), then on
	// MSG_INIT it follows InitData to the log ring, logs a line and rings.
	model = asctest.NewModel(func(m rtkit.Message) []rtkit.Message {
		if m.EP == 0 {
			switch m.Data >> 52 & 0xff {
			case 2: // HELLO reply -> EPMAP: 0x20, 0x21
				return []rtkit.Message{{EP: 0, Data: 8<<52 | 1<<51 | 1<<32 | 3}} // page 1
			case 8: // EPMAP reply -> IOP on
				return []rtkit.Message{{EP: 0, Data: 7<<52 | rtkit.PwrOn}}
			case 6: // IOP power request: acknowledge
				return []rtkit.Message{{EP: 0, Data: 7<<52 | m.Data&0xffff}}
			case 0xb: // AP power: echo
				return []rtkit.Message{{EP: 0, Data: m.Data}}
			}
			return nil
		}
		if m.EP == EPFirmware && m.Data>>48 == MsgInit>>48 {
			va := m.Data & initDataMask
			initVA.Store(va)
			// The InitData address is sign-extended: kernel VAs are high.
			root := gm.at(va|^uint64(initDataMask), L.Get("initdata.InitData").Size)
			rpVA := binary.LittleEndian.Uint64(root[L.Get("initdata.InitData").Off("runtime_pointers"):])
			rp := gm.at(rpVA, L.Get("initdata.RuntimePointers").Size)
			ringOff := L.Get("initdata.RuntimePointers").Off("fw_log")
			stVA := binary.LittleEndian.Uint64(rp[ringOff:])
			rgVA := binary.LittleEndian.Uint64(rp[ringOff+8:])
			bufVA := binary.LittleEndian.Uint64(rp[L.Get("initdata.RuntimePointers").Off("fwlog_buf"):])
			dc := L.Get("initdata.RuntimePointers").Off("device_control")
			dcState := gm.at(binary.LittleEndian.Uint64(rp[dc:]), 0x30)
			dcRing := gm.at(binary.LittleEndian.Uint64(rp[dc+8:]), DeviceControlMsgSize)
			if binary.LittleEndian.Uint32(dcState[0x20:]) == 1 { // one command queued
				devctl.Store(binary.LittleEndian.Uint32(dcRing))
			}
			// Sub-channel 1 (info): payload entry 0, then the message.
			pl := gm.at(bufVA+0x100*FwLogPayloadSize, FwLogPayloadSize)
			binary.LittleEndian.PutUint32(pl, 3)
			copy(pl[0x10:], "AGX: firmware initialized\x00")
			msg := gm.at(rgVA+0x100*FwLogMsgSize, FwLogMsgSize)
			binary.LittleEndian.PutUint32(msg, 2)
			st := gm.at(stVA, 2*0x30)
			binary.LittleEndian.PutUint32(st[0x30+0x20:], 1) // sub 1 write_ptr
			return []rtkit.Message{{EP: EPFirmware, Data: MsgRxDoorbell}}
		}
		return nil
	})
	ascR.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		if off == asc.CPU_CONTROL && v&asc.CPU_RUN != 0 && started.CompareAndSwap(false, true) {
			go fwh.run(fwCtx)                                           // the handoff
			model.Post(rtkit.Message{EP: 0, Data: 1<<52 | 12<<16 | 12}) // HELLO
		}
		return false
	}
	wins := map[uint64]mmio.Shared{c.ASC: ascR, c.SGX: sgx, c.Handoff.Base: ho, c.TTBs.Base: ttbs, 0: phys, c.Mailbox: model.F}
	dev.Register(Driver(func(b uint64) mmio.Shared {
		if w, ok := wins[b]; ok {
			return w
		}
		return mmio.NewFake()
	}))
	m := dev.NewManager(proc.NewTable(), nil, dma.New(dma.Region{Base: memBase, Mem: mem}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c.Power = nil
	m.Start(ctx, []dev.Node{{Name: "gpu0", Compatible: Compatible, Config: c}})
	svc, err := m.Get(ctx, "gpu0")
	if err != nil {
		t.Fatal(err)
	}
	g := svc.(*GPU)
	t0, t1 := NewTTBs(ttbs).Slot(0)
	if t1 != c.PageTables.Base|1 || t0&^(PageSize-1) < memBase || t0&1 != 1 {
		t.Fatalf("slot 0: ttb0 %#x ttb1 %#x", t0, t1)
	}
	lsdev := func() string { return strings.Join(g.Devices(), "\n") }
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(lsdev(), "firmware log: 1 lines") {
		if time.Now().After(deadline) {
			t.Fatalf("no firmware log:\n%s", lsdev())
		}
		time.Sleep(10 * time.Millisecond)
	}
	d := lsdev()
	for _, want := range []string{"G13S B1: 2 clusters × 8 cores, 16 active", "firmware 12.3.0 (compat 12.3.0)",
		"firmware initialized", "[info] AGX: firmware initialized", "doorbells 1", "HwDataA: no calibration blob"} {
		if !strings.Contains(d, want) {
			t.Fatalf("lsdev lacks %q:\n%s", want, d)
		}
	}
	if initVA.Load() == 0 {
		t.Fatal("no MSG_INIT")
	}
	// The device-control channel carried Initialize (0x19 on 12.3).
	if op := devctl.Load(); op != 0x19 {
		t.Fatalf("device control: command %#x, want Initialize (0x19)", op)
	}
	// A fault shows up once, however long it stays pending.
	sgx.Write64(FAULT_INFO, 0x4000>>6<<30|1)
	deadline = time.Now().Add(5 * time.Second)
	for !strings.Contains(lsdev(), "1 MMU faults") {
		if time.Now().After(deadline) {
			t.Fatalf("fault not reported:\n%s", lsdev())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(2500 * time.Millisecond)
	if d := lsdev(); !strings.Contains(d, "1 MMU faults") || !strings.Contains(d, "unmapped fault at 0x4000") {
		t.Fatalf("fault counted again or misreported:\n%s", d)
	}
	if v := fwh.violations.Load(); v != 0 {
		t.Fatalf("handoff lock violated %d times", v)
	}
}

func TestDriverRefusesUnknownFirmware(t *testing.T) {
	sgx := sgxFake(0xffff)
	d := &dev.Device{DMA: dma.New(dma.Region{Base: 1 << 40, Mem: make([]byte, 1<<16)})}
	err := run(context.Background(), d, func(b uint64) mmio.Shared {
		if b == 1 {
			return sgx
		}
		return mmio.NewFake()
	}, Config{ASC: 2, SGX: 1, Compat: [3]uint32{14, 0, 0}}, T6000)
	if err == nil || !strings.Contains(err.Error(), "14.0.0") {
		t.Fatalf("%v", err)
	}
}

// m1ProOPPs are the M1 Pro's GPU operating points (Asahi t600x-common.dtsi;
// m1n1 fills in the device's own voltages and power): Hz, µV, µW.
var m1ProOPPs = [][3]uint32{{0, 400000, 0}, {388800000, 634000, 25011450}, {486000000, 650000, 31681170},
	{648000000, 668000, 41685750}, {777600000, 715000, 56692620}, {972000000, 778000, 83371500},
	{1296000000, 903000, 166743000}}

// m1ProPower are t600x-die0.dtsi's power properties (floats as IEEE bits,
// as m1n1 writes them).
var m1ProPower = map[string]uint32{
	"apple,perf-base-pstate": 1, "apple,min-sram-microvolt": 790000, "apple,avg-power-filter-tc-ms": 1000,
	"apple,avg-power-ki-only": math.Float32bits(2.4), "apple,avg-power-kp": math.Float32bits(1.5),
	"apple,avg-power-min-duty-cycle": 40, "apple,avg-power-target-filter-tc": 125,
	"apple,fast-die0-integral-gain": math.Float32bits(500), "apple,fast-die0-proportional-gain": math.Float32bits(72),
	"apple,perf-boost-ce-step": 50, "apple,perf-boost-min-util": 90, "apple,perf-filter-drop-threshold": 0,
	"apple,perf-filter-time-constant": 5, "apple,perf-filter-time-constant2": 50,
	"apple,perf-integral-gain": math.Float32bits(6.3), "apple,perf-integral-gain2": math.Float32bits(0.197392),
	"apple,perf-integral-min-clamp": 0, "apple,perf-proportional-gain": math.Float32bits(15.75),
	"apple,perf-proportional-gain2": math.Float32bits(6.853981), "apple,perf-tgt-utilization": 85,
	"apple,power-sample-period": 8, "apple,ppm-filter-time-constant-ms": 100,
	"apple,ppm-ki": math.Float32bits(30), "apple,ppm-kp": math.Float32bits(1.5),
	"apple,pwr-filter-time-constant": 313, "apple,pwr-integral-gain": math.Float32bits(0.0202129),
	"apple,pwr-integral-min-clamp": 0, "apple,pwr-min-duty-cycle": 40,
	"apple,pwr-proportional-gain": math.Float32bits(5.2831855),
}
