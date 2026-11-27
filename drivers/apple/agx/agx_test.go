package agx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"godeep/drivers/apple/asc"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/fdt"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
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

func TestDriver(t *testing.T) {
	const (
		ascBase, sgxBase = 0x4_0640_0000, 0x4_0400_0000
		hoBase, ttbsBase = 0x10_0300_0000, 0x10_0301_4000
		ptBase           = 0x10_0300_4000
		memBase          = 0x108_0000_0000
	)
	sgx, ascR, ho, ttbs, phys := sgxFake(0xffff), mmio.NewFake(), mmio.NewFake(), mmio.NewFake(), mmio.NewFake()
	fw := &firmware{m: ho, answer: true}
	fwCtx, stopFW := context.WithCancel(context.Background())
	defer stopFW()
	var started atomic.Bool
	ascR.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		if off == asc.CPU_CONTROL && v&asc.CPU_RUN != 0 && started.CompareAndSwap(false, true) {
			go fw.run(fwCtx) // the coprocessor starts: the firmware answers
		}
		return false
	}
	wins := map[uint64]mmio.Shared{ascBase: ascR, sgxBase: sgx, hoBase: ho, ttbsBase: ttbs, 0: phys}
	dev.Register(Driver(func(b uint64) mmio.Shared {
		if w, ok := wins[b]; ok {
			return w
		}
		return mmio.NewFake()
	}))
	m := dev.NewManager(proc.NewTable(), nil, dma.New(dma.Region{Base: memBase, Mem: make([]byte, 1<<20)}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := Config{ASC: ascBase, SGX: sgxBase, Handoff: Region{hoBase, 0x4000}, TTBs: Region{ttbsBase, 0x4000},
		PageTables: Region{ptBase, 0x10000}, Firmware: [3]uint32{12, 3, 1}, Compat: [3]uint32{12, 3, 0}}
	m.Start(ctx, []dev.Node{{Name: "gpu0", Compatible: Compatible, Config: cfg}})
	svc, err := m.Get(ctx, "gpu0")
	if err != nil {
		t.Fatal(err)
	}
	g := svc.(*GPU)
	if !started.Load() {
		t.Fatal("coprocessor CPU never started")
	}
	tt := NewTTBs(ttbs)
	t0, t1 := tt.Slot(0)
	if t1 != ptBase|1 || t0&^(PageSize-1) < memBase || t0&1 != 1 {
		t.Fatalf("slot 0: ttb0 %#x ttb1 %#x", t0, t1)
	}
	d := strings.Join(g.Devices(), "\n")
	for _, want := range []string{"G13S B1: 2 clusters × 8 cores, 16 active", "firmware 12.3.1 (compat 12.3.0)", "UAT handed off", "0 MMU faults"} {
		if !strings.Contains(d, want) {
			t.Fatalf("lsdev lacks %q:\n%s", want, d)
		}
	}
	// A fault shows up once, however long it stays pending.
	sgx.Write64(FAULT_INFO, 0x4000>>6<<30|1)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(g.Devices(), "\n"), "1 MMU faults") {
		if time.Now().After(deadline) {
			t.Fatalf("fault not reported:\n%s", strings.Join(g.Devices(), "\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(2500 * time.Millisecond)
	if d := strings.Join(g.Devices(), "\n"); !strings.Contains(d, "1 MMU faults") || !strings.Contains(d, "unmapped fault at 0x4000") {
		t.Fatalf("fault counted again or misreported:\n%s", d)
	}
	if v := fw.violations.Load(); v != 0 {
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
