package ans

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"godeep/drivers/apple/asc/asctest"
	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/fdt"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
	"godeep/kernel/rtkit"
)

const (
	memBase  = 0x101_4020_0000
	nvmeBase = 0x3_93cc_0000
	ansBase  = 0x3_8f40_0000
	mboxBase = 0x3_8f40_8000
	sartBase = 0x3_93c5_0000
	blocks   = 1 << 20
)

// ssd models ANS2: RTKit over the mailbox, then NVMe with linear
// submission and TCBs.
type ssd struct {
	t        *testing.T
	mu       sync.Mutex
	mem      []byte
	nvme     *mmio.Fake
	mbox     *asctest.Model
	cqs      map[int]uint64 // queue id -> CQ address
	sqs      map[int]uint64
	tcbs     map[int]uint64 // 0 admin, 1 I/O
	heads    map[int]int
	phase    map[int]uint32
	cmdsSeen int
}

func (s *ssd) at(pa uint64, n int) []byte { return s.mem[pa-memBase:][:n] }

func newSSD(t *testing.T, mem []byte) *ssd {
	s := &ssd{t: t, mem: mem, nvme: mmio.NewFake(), cqs: map[int]uint64{}, sqs: map[int]uint64{},
		tcbs: map[int]uint64{}, heads: map[int]int{}, phase: map[int]uint32{0: 1, 1: 1}}
	s.mbox = asctest.NewModel(func(m rtkit.Message) []rtkit.Message {
		switch m.Data >> 52 & 0xff {
		case 2: // HELLO reply -> EPMAP (last, no endpoints)
			return []rtkit.Message{{EP: 0, Data: 8<<52 | 1<<51}}
		case 8: // EPMAP reply -> IOP on; the firmware then says it booted
			s.nvme.Set(BOOT_STATUS, BootOK)
			return []rtkit.Message{{EP: 0, Data: 7<<52 | rtkit.PwrOn}}
		case 0xb:
			return []rtkit.Message{{EP: 0, Data: m.Data}}
		case 6:
			return []rtkit.Message{{EP: 0, Data: 7<<52 | m.Data&0xffff}}
		}
		return nil
	})
	s.mbox.Post(rtkit.Message{EP: 0, Data: 1<<52 | 12<<16 | 11})
	s.nvme.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		switch off {
		case CC:
			if v&CC_EN != 0 {
				f.Set(CSTS, CSTS_RDY)
			} else {
				f.Set(CSTS, 0)
			}
			if v&CC_SHN_MASK == CC_SHN_NORMAL {
				f.Set(CSTS, f.Get(CSTS)|CSTS_SHST_DONE)
			}
		case DB_LINEAR_ASQ:
			s.exec(0, int(v))
		case DB_LINEAR_IOQ:
			s.exec(1, int(v))
		}
		return false
	}
	return s
}

func (s *ssd) reg64(off uint64) uint64 {
	return uint64(s.nvme.Get(off)) | uint64(s.nvme.Get(off+4))<<32
}

func (s *ssd) exec(q, tag int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	le := binary.LittleEndian
	sq, tcbBase := s.reg64(ASQ), s.reg64(NVMMU_ASQ)
	cq := s.reg64(ACQ)
	if q == 1 {
		sq, tcbBase, cq = s.sqs[1], s.reg64(NVMMU_IOSQ), s.cqs[1]
	}
	c := s.at(sq+uint64(tag*64), 64)
	tcb := s.at(tcbBase+uint64(tag*128), 128)
	op, prp1, prp2 := c[0], le.Uint64(c[24:]), le.Uint64(c[32:])
	if le.Uint64(tcb[24:]) != prp1 || le.Uint64(tcb[32:]) != prp2 || int(tcb[2]) != tag {
		s.t.Errorf("TCB doesn't match command %#x", op)
	}
	wantFlags := byte(tcbFromDevice)
	if op&1 != 0 { // NVMe: odd opcodes send data to the device
		wantFlags = tcbToDevice
	}
	if prp1 != 0 && tcb[1] != wantFlags {
		s.t.Errorf("TCB DMA flags %#x for command %#x", tcb[1], op)
	}
	s.cmdsSeen++
	status := uint32(0)
	switch {
	case q == 0 && op == opCreateCQ:
		s.cqs[int(le.Uint32(c[40:])&0xffff)] = prp1
	case q == 0 && op == opCreateSQ:
		s.sqs[int(le.Uint32(c[40:])&0xffff)] = prp1
	case q == 0 && op == opIdentify && le.Uint32(c[40:]) == 1:
		id := s.at(prp1, 4096)
		clear(id)
		copy(id[24:], "APPLE SSD AP0512R                       ")
		copy(id[64:], "1234.56 ")
	case q == 0 && op == opIdentify:
		id := s.at(prp1, 4096)
		clear(id)
		le.PutUint64(id, blocks)
		le.PutUint32(id[128:], 12<<16) // LBA format 0: 4096-byte blocks
	case q == 1 && op == opRead:
		lba := le.Uint64(c[40:])
		n := int(le.Uint32(c[48:])&0xffff) + 1
		pages := []uint64{prp1}
		switch {
		case n == 2:
			pages = append(pages, prp2)
		case n > 2:
			for i := 0; i < n-1; i++ {
				pages = append(pages, le.Uint64(s.at(prp2+uint64(8*i), 8)))
			}
		}
		if int(le.Uint32(tcb[4:])) != n-1 {
			s.t.Errorf("TCB length %d for %d blocks", le.Uint32(tcb[4:]), n)
		}
		for i, pa := range pages {
			fill(s.at(pa, 4096), lba+uint64(i))
		}
	case q == 0 && (op == opDeleteSQ || op == opDeleteCQ):
	default:
		status = 1 // invalid opcode
	}
	e := s.at(cq+uint64(s.heads[q]*16), 16)
	le.PutUint32(e[0:], 0)
	le.PutUint32(e[12:], uint32(tag)|(status<<1|s.phase[q])<<16)
	if s.heads[q]++; s.heads[q] == QueueSize {
		s.heads[q], s.phase[q] = 0, s.phase[q]^1
	}
}

func fill(p []byte, lba uint64) {
	for i := 0; i < len(p); i += 8 {
		binary.LittleEndian.PutUint64(p[i:], lba<<16|uint64(i))
	}
}

func TestBootIdentifyRead(t *testing.T) {
	mem := make([]byte, 8<<20)
	s := newSSD(t, mem)
	fakes := map[uint64]mmio.Regs64{nvmeBase: s.nvme, mboxBase: s.mbox.F, ansBase: mmio.NewFake(), sartBase: mmio.NewFake()}
	dev.Register(Driver(func(b uint64) mmio.Regs64 {
		if f, ok := fakes[b]; ok {
			return f
		}
		return mmio.NewFake()
	}))
	m := dev.NewManager(proc.NewTable(), nil, dma.New(dma.Region{Base: memBase, Mem: mem}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m.Start(ctx, []dev.Node{{Name: "nvme@393cc0000", Compatible: Compatible,
		Config: Config{NVMe: nvmeBase, ANS: ansBase, Mailbox: mboxBase, SART: sartBase}}})
	svc, err := m.Get(ctx, "nvme@393cc0000")
	if err != nil {
		t.Fatal(err)
	}
	k := svc.(*Disk)
	if _, ok := svc.(block.Writer); ok {
		t.Fatal("the SSD must be read-only")
	}
	if k.BlockSize() != 4096 || k.Blocks() != blocks {
		t.Fatalf("%d blocks of %d", k.Blocks(), k.BlockSize())
	}
	if got := k.Devices()[0]; !bytes.Contains([]byte(got), []byte("APPLE SSD AP0512R (firmware 1234.56), namespace 1: 4 GB")) {
		t.Fatalf("info %q", got)
	}
	for _, n := range []int{1, 2, 3, 16, 17, 40} {
		p := make([]byte, n*4096)
		if err := k.ReadBlocks(ctx, 100, p); err != nil {
			t.Fatalf("%d blocks: %v", n, err)
		}
		want := make([]byte, 4096)
		for i := 0; i < n; i++ {
			fill(want, 100+uint64(i))
			if !bytes.Equal(p[i*4096:(i+1)*4096], want) {
				t.Fatalf("%d-block read: block %d wrong", n, i)
			}
		}
	}
	if err := k.ReadBlocks(ctx, blocks-1, make([]byte, 8192)); err != block.ErrRange {
		t.Fatalf("read past the end: %v", err)
	}
	if got := fakes[sartBase].(*mmio.Fake).Get(0); got != 0xff {
		t.Fatalf("SART entry 0 config %#x: DMA region not allowed", got)
	}
	cancel()
}

func TestConfigFromFDT(t *testing.T) {
	var b fdt.Builder
	b.Begin("")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Begin("power-management@28e080000")
	b.PropRegs(2, 2, fdt.Range{Base: 0x2_8e08_0000, Size: 0x4000})
	b.PropU32("#address-cells", 1)
	b.PropU32("#size-cells", 1)
	b.Begin("power-controller@3c8")
	b.PropStrings("compatible", "apple,pmgr-pwrstate")
	b.PropU32("reg", 0x3c8, 8)
	b.PropU32("#power-domain-cells", 0)
	b.PropU32("#reset-cells", 0)
	b.PropU32("phandle", 1)
	b.End()
	b.End()
	b.Begin("mbox@38f408000")
	b.PropRegs(2, 2, fdt.Range{Base: mboxBase, Size: 0x4000})
	b.PropU32("#mbox-cells", 0)
	b.PropU32("power-domains", 1)
	b.PropU32("phandle", 2)
	b.End()
	b.Begin("sart@393c50000")
	b.PropRegs(2, 2, fdt.Range{Base: sartBase, Size: 0x10000})
	b.PropU32("power-domains", 1)
	b.PropU32("phandle", 3)
	b.End()
	b.Begin("nvme@393cc0000")
	b.PropStrings("compatible", Compatible, "apple,nvme-ans2")
	b.PropRegs(2, 2, fdt.Range{Base: nvmeBase, Size: 0x40000}, fdt.Range{Base: ansBase, Size: 0x4000})
	b.PropStrings("reg-names", "nvme", "ans")
	b.PropU32("mboxes", 2)
	b.PropU32("apple,sart", 3)
	b.PropU32("power-domains", 1)
	b.PropU32("resets", 1)
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
	c, err := ConfigFromFDT(root.Find("/nvme@393cc0000"))
	if err != nil {
		t.Fatal(err)
	}
	if c.NVMe != nvmeBase || c.ANS != ansBase || c.Mailbox != mboxBase || c.SART != sartBase {
		t.Fatalf("%+v", c)
	}
	if len(c.Power) != 1 || c.Reset == nil || c.Reset.Addr != 0x2_8e08_03c8 {
		t.Fatalf("power %+v reset %+v", c.Power, c.Reset)
	}
}
