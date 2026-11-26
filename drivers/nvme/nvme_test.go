package nvme

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"godeep/drivers/pci"
	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
)

const dmaBase = 0x1_2000_0000

// model is an NVMe controller: registers, and commands executed when a
// submission doorbell rings, reading and writing "physical" memory (the
// DMA region's bytes).
type model struct {
	mu      sync.Mutex
	f       *mmio.Fake
	mem     []byte // DMA region; physical address dmaBase
	disk    []byte // namespace 1 contents
	bs      int
	mdts    byte
	queues  map[int]*mq
	reads   int
	maxXfer int
}

type mq struct {
	sq, cq      uint64 // physical
	size        int
	head, ctail int
	phase       uint32
}

func newModel(disk []byte, bs int, mdts byte) *model {
	m := &model{f: mmio.NewFake(), mem: make([]byte, 16<<20), disk: disk, bs: bs, mdts: mdts, queues: map[int]*mq{}}
	m.f.Set(CAP, 63|20<<24) // MQES 64 entries, 10 s timeout
	m.f.Set(CAP+4, 1<<5)    // CSS: NVM command set (bit 37)
	m.f.Set(VS, 1<<16|4<<8) // 1.4
	m.f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case off == CC:
			f.Set(CC, v)
			rdy := uint32(0)
			if v&CC_EN != 0 {
				rdy = CSTS_RDY
				m.queues[0] = &mq{sq: m.reg64(ASQ), cq: m.reg64(ACQ), size: int(f.Get(AQA)&0xfff) + 1, phase: 1}
			}
			if v&CC_SHN_NM != 0 {
				rdy |= 2 << 2 // shutdown complete
			}
			f.Set(CSTS, rdy)
			return true
		case off >= DB && (off-DB)/4%2 == 0: // submission tail doorbell
			q := m.queues[int((off-DB)/8)]
			for q.head != int(v) {
				m.exec(q, m.mem[q.sq-dmaBase+uint64(q.head*64):][:64])
				q.head = (q.head + 1) % q.size
			}
			return true
		}
		return false
	}
	return m
}

func (m *model) reg64(off uint64) uint64 { return uint64(m.f.Get(off)) | uint64(m.f.Get(off+4))<<32 }

func (m *model) at(phys uint64, n int) []byte { return m.mem[phys-dmaBase:][:n] }

func (m *model) exec(q *mq, e []byte) {
	le := binary.LittleEndian
	op, cid := e[0], le.Uint16(e[2:])
	prp1, prp2 := le.Uint64(e[24:]), le.Uint64(e[32:])
	cdw10, cdw11, cdw12 := le.Uint32(e[40:]), le.Uint32(e[44:]), le.Uint32(e[48:])
	status := uint16(0)
	switch {
	case q == m.queues[0] && op == opIdentify && cdw10 == 1:
		id := m.at(prp1, 4096)
		clear(id)
		copy(id[4:], "godeep-serial       ")
		copy(id[24:], "Model NVMe                              ")
		id[77] = m.mdts
		le.PutUint32(id[516:], 1)
	case q == m.queues[0] && op == opIdentify && cdw10 == 0:
		ns := m.at(prp1, 4096)
		clear(ns)
		le.PutUint64(ns[0:], uint64(len(m.disk)/m.bs))
		lbads := 9
		for 1<<lbads < m.bs {
			lbads++
		}
		le.PutUint32(ns[128:], uint32(lbads)<<16)
	case q == m.queues[0] && op == opCreateCQ:
		m.queues[int(cdw10&0xffff)] = &mq{cq: prp1, size: int(cdw10>>16) + 1, phase: 1}
	case q == m.queues[0] && op == opCreateSQ:
		qq := m.queues[int(cdw11>>16)]
		qq.sq = prp1
		m.queues[int(cdw10&0xffff)] = qq
	case q != m.queues[0] && op == opWrite:
		lba := uint64(cdw10) | uint64(cdw11)<<32
		n := (int(cdw12&0xffff) + 1) * m.bs
		pages := []uint64{prp1}
		switch np := (n + 4095) / 4096; {
		case np == 2:
			pages = append(pages, prp2)
		case np > 2:
			list := m.at(prp2, 4096)
			for i := 0; i < np-1; i++ {
				pages = append(pages, le.Uint64(list[8*i:]))
			}
		}
		dst := m.disk[lba*uint64(m.bs):][:n]
		for i, p := range pages {
			copy(dst[min(i*4096, n):min((i+1)*4096, n)], m.at(p, 4096))
		}
	case q != m.queues[0] && op == opRead:
		lba := uint64(cdw10) | uint64(cdw11)<<32
		n := (int(cdw12&0xffff) + 1) * m.bs
		m.maxXfer = max(m.maxXfer, n)
		src := m.disk[lba*uint64(m.bs):][:n]
		// PRPs: page 1 at prp1, then prp2 (one page) or a list
		pages := []uint64{prp1}
		switch np := (n + 4095) / 4096; {
		case np == 2:
			pages = append(pages, prp2)
		case np > 2:
			list := m.at(prp2, 4096)
			for i := 0; i < np-1; i++ {
				pages = append(pages, le.Uint64(list[8*i:]))
			}
		}
		for i, p := range pages {
			copy(m.at(p, 4096), src[min(i*4096, n):min((i+1)*4096, n)])
		}
		m.reads++
	default:
		status = 1 << 1 // invalid opcode
	}
	c := m.at(q.cq+uint64(q.ctail*16), 16)
	le.PutUint16(c[8:], uint16(q.head))
	le.PutUint16(c[12:], cid)
	le.PutUint16(c[14:], status|uint16(q.phase))
	q.ctail++
	if q.ctail == q.size {
		q.ctail, q.phase = 0, q.phase^1
	}
}

func start(t *testing.T, m *model) (*Disk, context.CancelFunc) {
	t.Helper()
	dev.Register(Driver(func(uint64) mmio.Regs { return m.f }))
	alloc := dma.New(dma.Region{Base: dmaBase, Mem: m.mem})
	mgr := dev.NewManager(proc.NewTable(), nil, alloc)
	ctx, cancel := context.WithCancel(context.Background())
	f := &pci.Function{Class: 0x010802}
	f.BARs[0] = pci.BAR{CPU: 0x10000000, Size: 0x4000}
	mgr.Start(ctx, []dev.Node{{Name: "nvme0", Compatible: Compatible, Config: f}})
	svc, err := mgr.Get(ctx, "nvme0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return svc.(*Disk), cancel
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i>>9)
	}
	return b
}

func TestReads(t *testing.T) {
	disk := pattern(4 << 20)
	m := newModel(disk, 512, 0)
	d, cancel := start(t, m)
	defer cancel()
	if d.BlockSize() != 512 || d.Blocks() != 8192 {
		t.Fatalf("%d blocks of %d", d.Blocks(), d.BlockSize())
	}
	if info := d.Devices()[0]; info != "Model NVMe (serial godeep-serial), NVMe 1.4, namespace 1: 4 MiB, 512-byte blocks" {
		t.Fatalf("info %q", info)
	}
	ctx := context.Background()
	// 1 block, 2 pages, a PRP list, and more than one transfer
	for _, c := range []struct{ lba, n int }{{0, 1}, {7, 16}, {100, 64}, {1000, 600}} {
		p := make([]byte, c.n*512)
		if err := d.ReadBlocks(ctx, uint64(c.lba), p); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(p, disk[c.lba*512:][:c.n*512]) {
			t.Fatalf("read %d blocks at %d: wrong data", c.n, c.lba)
		}
	}
	if m.maxXfer > maxTransfer {
		t.Fatalf("transfer of %d bytes", m.maxXfer)
	}
	if err := d.ReadBlocks(ctx, 8190, make([]byte, 4*512)); err != block.ErrRange {
		t.Fatalf("read past the end: %v", err)
	}

	// concurrent readers through the cached block reader
	r := block.NewReader(ctx, d)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := make([]byte, 70_000)
			off := int64(i) * 400_000
			if _, err := r.ReadAt(p, off); err != nil || !bytes.Equal(p, disk[off:off+70_000]) {
				t.Errorf("reader %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	// wrap around the queues several times
	for i := 0; i < 3*queueDepth; i++ {
		if err := d.ReadBlocks(ctx, uint64(i), make([]byte, 512)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMDTSAndBlockSize(t *testing.T) {
	disk := pattern(2 << 20)
	m := newModel(disk, 4096, 1) // 4 KB blocks, transfers of at most 8 KB
	d, cancel := start(t, m)
	defer cancel()
	p := make([]byte, 40<<10)
	if err := d.ReadBlocks(context.Background(), 3, p); err != nil || !bytes.Equal(p, disk[3*4096:][:40<<10]) {
		t.Fatalf("read: %v", err)
	}
	if m.maxXfer != 8192 {
		t.Fatalf("largest transfer %d, want 8192 (MDTS)", m.maxXfer)
	}
}

func TestShutdownFreesDMA(t *testing.T) {
	m := newModel(pattern(1<<20), 512, 0)
	_, cancel := start(t, m)
	cancel()
	// the driver shuts the controller down on its way out
	for i := 0; i < 1000 && m.f.Get(CSTS)&CSTS_SHST == 0; i++ {
		sleep(context.Background(), 1e6)
	}
	if m.f.Get(CSTS)&CSTS_SHST != 2<<2 {
		t.Fatal("controller not shut down")
	}
}

func TestWrites(t *testing.T) {
	disk := make([]byte, 4<<20)
	m := newModel(disk, 512, 0)
	d, cancel := start(t, m)
	defer cancel()
	ctx := context.Background()
	data := pattern(300 << 10) // more than one transfer, PRP lists
	if err := d.WriteBlocks(ctx, 7, data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk[7*512:7*512+len(data)], data) {
		t.Fatal("disk contents differ")
	}
	back := make([]byte, len(data))
	if err := d.ReadBlocks(ctx, 7, back); err != nil || !bytes.Equal(back, data) {
		t.Fatalf("read back: %v", err)
	}
	var _ block.Writer = d
}
