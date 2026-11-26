package pci

import (
	"context"
	"strings"
	"sync"
	"testing"

	"godeep/kernel/dev"
	"godeep/kernel/fdt"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
)

// fn models one function's config space with sizable BARs.
type fn struct {
	id, class, header uint32
	mask, ro          [6]uint32 // per BAR register: writable bits, read-only bits (0, 0: unimplemented)
	val               [6]uint32
	cmd               uint32
}

// ecam models config space: functions by (dev, fn), reads of absent ones
// return all ones.
type ecam struct {
	mu  sync.Mutex
	fns map[[2]int]*fn
}

func (e *ecam) Read32(off uint64) uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.fns[[2]int{int(off >> 15 & 31), int(off >> 12 & 7)}]
	if f == nil || off>>20 != 0 {
		return 0xffffffff
	}
	switch r := off & 0xfff; {
	case r == VENDOR_ID:
		return f.id
	case r == COMMAND:
		return f.cmd | 0x0010<<16 // capabilities list in status
	case r == CLASS:
		return f.class<<8 | 1
	case r == HEADER_TYPE:
		return f.header << 16
	case r >= BAR0 && r < BAR0+24:
		return f.val[(r-BAR0)/4]
	}
	return 0
}

func (e *ecam) Write32(off uint64, v uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.fns[[2]int{int(off >> 15 & 31), int(off >> 12 & 7)}]
	if f == nil {
		return
	}
	switch r := off & 0xfff; {
	case r == COMMAND:
		if v>>16 != 0 {
			panic("wrote 1s to the write-1-to-clear status register")
		}
		f.cmd = v & 0xffff
	case r >= BAR0 && r < BAR0+24:
		i := (r - BAR0) / 4
		f.val[i] = v&f.mask[i] | f.ro[i]
	}
}

// bar64 makes BARs i and i+1 one 64-bit memory BAR of size bytes.
func (f *fn) bar64(i int, size uint32) {
	f.mask[i], f.ro[i], f.val[i] = ^(size - 1), BAR_TYPE_64, BAR_TYPE_64
	f.mask[i+1] = 0xffffffff
}

// bar32 makes BAR i a 32-bit memory BAR (flags: e.g. BAR_PREFETCH).
func (f *fn) bar32(i int, size, flags uint32) {
	f.mask[i], f.ro[i], f.val[i] = ^(size - 1), flags, flags
}

func newECAM() *ecam {
	nvme := &fn{id: 0x0010_1b36, class: 0x010802}
	nvme.bar64(0, 0x4000) // 16 KB
	mf0 := &fn{id: 0x1000_1af4, class: 0x020000, header: 0x80}
	mf0.bar32(1, 0x1000, 0) // 4 KB in BAR1
	io := &fn{id: 0x2000_1af4, class: 0x0c0330}
	io.mask[0], io.ro[0], io.val[0] = 0xffffffe0, BAR_IO, BAR_IO // an I/O BAR: skipped
	io.bar32(2, 0x10000, BAR_PREFETCH)
	return &ecam{fns: map[[2]int]*fn{
		{0, 0}: {id: 0x0008_1b36, class: 0x060000},
		{1, 0}: nvme,
		{2, 0}: mf0,
		{2, 3}: io,
		{4, 0}: {id: 0x000c_1b36, class: 0x060400, header: 1},
	}}
}

var windows = []Window{
	{Space: SpaceIO, PCI: 0, CPU: 0x3eff0000, Size: 0x10000},
	{Space: SpaceMem32, PCI: 0x10000000, CPU: 0x10000000, Size: 0x2eff0000},
	{Space: SpaceMem64, PCI: 0x80_0000_0000, CPU: 0x80_0000_0000, Size: 0x80_0000_0000},
}

func TestScan(t *testing.T) {
	e := newECAM()
	fns, err := Scan(e, windows)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range fns {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, " "); got != "host0 nvme0 net0 xhci0 pci0" {
		t.Fatalf("functions: %s", got)
	}
	nvme := fns[1]
	if b := nvme.BARs[0]; b.Size != 0x4000 || !b.Is64 || b.CPU%0x4000 != 0 || b.CPU < 0x10000000 {
		t.Fatalf("nvme BAR0 %+v", b)
	}
	if e.fns[[2]int{1, 0}].val[0]&^0xf != uint32(nvme.BARs[0].PCI) || e.fns[[2]int{1, 0}].val[1] != 0 {
		t.Fatal("BAR0 not programmed")
	}
	if e.fns[[2]int{1, 0}].cmd != CMD_MEMORY|CMD_BUS_MASTER {
		t.Fatalf("command %#x", e.fns[[2]int{1, 0}].cmd)
	}
	if b := fns[2].BARs[1]; b.Size != 0x1000 || b.Is64 {
		t.Fatalf("net0 BAR1 %+v", b)
	}
	x := fns[3]
	if x.Fn != 3 || x.BARs[0].Size != 0 || x.BARs[2].Size != 0x10000 || !x.BARs[2].Prefetch {
		t.Fatalf("xhci0 %+v", x)
	}
	// no overlaps
	type r struct{ a, b uint64 }
	var rs []r
	for _, f := range fns {
		for _, b := range f.BARs {
			if b.Size == 0 {
				continue
			}
			for _, o := range rs {
				if b.CPU < o.b && o.a < b.CPU+b.Size {
					t.Fatalf("BARs overlap: %s", f)
				}
			}
			rs = append(rs, r{b.CPU, b.CPU + b.Size})
		}
	}
	if !fns[4].Bridge || !strings.Contains(fns[4].String(), "bridge") {
		t.Fatal("bridge not reported")
	}
}

func TestDriverAddsFunctions(t *testing.T) {
	e := newECAM()
	dev.Register(Driver(func(uint64) mmio.Regs { return e }))
	m := dev.NewManager(proc.NewTable(), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx, []dev.Node{{Name: "pcie@10000000", Compatible: Compatible,
		Config: HostConfig{ECAM: 0x40_1000_0000, Windows: windows}}})
	svc, err := m.Get(ctx, "pcie@10000000")
	if err != nil {
		t.Fatal(err)
	}
	if l := svc.(dev.Lister).Devices(); len(l) != 5 {
		t.Fatalf("listed %v", l)
	}
	found := map[string]string{}
	for _, s := range m.List() {
		found[s.Name] = s.Compatible
	}
	if found["nvme0"] != "pciclass,010802" || found["xhci0"] != "pciclass,0c0330" {
		t.Fatalf("devices %v", found)
	}
	if _, ok := found["host0"]; ok {
		t.Fatal("host bridge added as a device")
	}
}

func TestConfigFromFDT(t *testing.T) {
	// as QEMU virt describes its PCIe host (highmem ECAM)
	var b fdt.Builder
	b.Begin("")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Begin("pcie@10000000")
	b.PropStrings("compatible", Compatible)
	b.PropU32("#address-cells", 3)
	b.PropU32("#size-cells", 2)
	b.PropU32("bus-range", 0, 0xff)
	b.PropRegs(2, 2, fdt.Range{Base: 0x40_1000_0000, Size: 0x1000_0000})
	b.PropU32("ranges",
		0x01000000, 0, 0, 0, 0x3eff0000, 0, 0x10000,
		0x02000000, 0, 0x10000000, 0, 0x10000000, 0, 0x2eff0000,
		0x43000000, 0x80, 0, 0x80, 0, 0x80, 0)
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
	c, err := ConfigFromFDT(root.Find("/pcie@10000000"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ECAM != 0x40_1000_0000 || len(c.Windows) != 3 {
		t.Fatalf("%+v", c)
	}
	if w := c.Windows[1]; w.Space != SpaceMem32 || w.PCI != 0x10000000 || w.CPU != 0x10000000 || w.Size != 0x2eff0000 {
		t.Fatalf("mem32 %+v", w)
	}
	if w := c.Windows[2]; w.Space != SpaceMem64 || !w.Prefetch || w.CPU != 0x80_0000_0000 {
		t.Fatalf("mem64 %+v", w)
	}
}
