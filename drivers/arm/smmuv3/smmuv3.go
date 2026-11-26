// Package smmuv3 sets up an Arm SMMUv3 (QEMU virt's "iommu=smmuv3", in
// front of its PCIe bus) the way godeep uses Apple's DART: the board's
// whole DMA region mapped once, linearly, at a bus window, so drivers hand
// devices dma.Buffer.Bus addresses (dma.Allocator.Translated). On QEMU it
// is the rehearsal of the Mac's IOMMUs: a driver that gives a device a
// physical address instead faults here, visibly, in the event queue.
//
// Minimal by design: a linear stream table (every stream ID below 2^n),
// stage-1 translation through one context descriptor shared by all
// streams, 4 KB-granule tables with 2 MB blocks, command and event queues
// polled. No ATS, no PRI, no MSIs.
package smmuv3

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

// Compatible is the device-tree string.
const Compatible = "arm,smmu-v3"

// Registers (page 0, and page 1 at +0x10000 for the event queue indexes).
const (
	IDR0            = 0x00
	IDR1            = 0x04
	IDR5            = 0x14
	CR0             = 0x20
	CR0ACK          = 0x24
	CR1             = 0x28
	CR2             = 0x2c
	GERROR          = 0x60
	STRTAB_BASE     = 0x80 // 64-bit
	STRTAB_BASE_CFG = 0x88
	CMDQ_BASE       = 0x90 // 64-bit
	CMDQ_PROD       = 0x98
	CMDQ_CONS       = 0x9c
	EVENTQ_BASE     = 0xa0 // 64-bit
	EVENTQ_PROD     = 0x100a8
	EVENTQ_CONS     = 0x100ac

	CR0_SMMUEN   = 1 << 0
	CR0_EVENTQEN = 1 << 2
	CR0_CMDQEN   = 1 << 3

	IDR0_S1P = 1 << 1

	cmdCfgiAll  = 0x04
	cmdTlbiNsnh = 0x30
	cmdSync     = 0x46

	queueLog2 = 6 // 64 entries
	block2M   = 2 << 20
	pteBlock  = 1 // level 2 block descriptor
	pteTable  = 3
	pteAF     = 1 << 10
	pteSHInn  = 3 << 8
	pteAPUnpr = 1 << 6 // EL0 access: SMMU transactions are unprivileged
	pteNG     = 1 << 11
	mairWB    = 0xff // attribute 0: normal, write-back
)

// SMMU is a configured SMMUv3.
type SMMU struct {
	r       mmio.Regs
	bus     uint64 // where the region appears to devices
	size    int
	sids    int
	allocs  []*dma.Buffer
	cmdq    *dma.Buffer
	cmdProd uint32
	evq     *dma.Buffer
	faults  []string
}

func write64(r mmio.Regs, off, v uint64) {
	r.Write32(off, uint32(v))
	r.Write32(off+4, uint32(v>>32))
}

// Setup maps a's region at bus (2 MB aligned, below 4 GB) for stream IDs
// below 2^log2SIDs and turns translation on. Page tables, queues and the
// stream table come from a (the SMMU walks physical memory).
func Setup(r mmio.Regs, a *dma.Allocator, bus uint64, log2SIDs int) (*SMMU, error) {
	if r.Read32(IDR0)&IDR0_S1P == 0 {
		return nil, errors.New("smmuv3: no stage-1 translation")
	}
	base, size := a.Region()
	size &^= block2M - 1
	if base%block2M != 0 || bus%block2M != 0 || bus+uint64(size) > 1<<32 {
		return nil, fmt.Errorf("smmuv3: region %#x+%#x at bus %#x: not 2 MB aligned or past 4 GB", base, size, bus)
	}
	s := &SMMU{r: r, bus: bus, size: size, sids: 1 << log2SIDs}
	alloc := func(n, align int) (*dma.Buffer, error) {
		b, err := a.Alloc(n, align)
		if err == nil {
			s.allocs = append(s.allocs, b)
		}
		return b, err
	}

	// Off first.
	r.Write32(CR0, 0)
	if err := s.waitAck(0); err != nil {
		return nil, err
	}

	// Page tables: one level-1 table (4 entries cover 4 GB) pointing at one
	// level-2 table of 2 MB blocks.
	l1, err := alloc(4096, 4096)
	if err != nil {
		return nil, err
	}
	l2, err := alloc(4096, 4096)
	if err != nil {
		return nil, err
	}
	if bus>>30 != (bus+uint64(size)-1)>>30 {
		return nil, errors.New("smmuv3: the bus window crosses a 1 GB boundary")
	}
	binary.LittleEndian.PutUint64(l1.Bytes[8*(bus>>30):], l2.Phys|pteTable)
	for off := uint64(0); off < uint64(size); off += block2M {
		i := (bus + off) >> 21 & 511
		binary.LittleEndian.PutUint64(l2.Bytes[8*i:], (base+off)|pteBlock|pteAF|pteSHInn|pteAPUnpr|pteNG)
	}

	// One context descriptor (64 bytes), shared by every stream.
	cd, err := alloc(64, 64)
	if err != nil {
		return nil, err
	}
	const t0sz = 32 // 4 GB of IOVA: the walk starts at level 1
	cd0 := uint64(t0sz) | 0<<6 /*TG0 4K*/ | 1<<8 | 1<<10 /*IR0, OR0 WBRA*/ | 3<<12 /*SH0 inner*/ |
		1<<30 /*EPD1*/ | 1<<31 /*V*/ | 5<<32 /*IPS 48-bit*/ | 1<<35 /*AFFD*/ | 1<<41 /*AA64*/ |
		1<<45 /*R*/ | 1<<46 /*A*/ | 1<<47 /*ASET*/
	binary.LittleEndian.PutUint64(cd.Bytes[0:], cd0)
	binary.LittleEndian.PutUint64(cd.Bytes[8:], l1.Phys&^0xf) // TTB0
	binary.LittleEndian.PutUint64(cd.Bytes[24:], mairWB)      // MAIR0

	// Linear stream table: every STE translates through the CD.
	st, err := alloc(64*s.sids, 64*s.sids)
	if err != nil {
		return nil, err
	}
	ste0 := uint64(1) /*V*/ | 5<<1 /*S1 translate, S2 bypass*/ | cd.Phys&^0x3f
	ste1 := uint64(1) << 44 // SHCFG: use incoming; STRW 0 (NS-EL1)
	for i := 0; i < s.sids; i++ {
		binary.LittleEndian.PutUint64(st.Bytes[64*i:], ste0)
		binary.LittleEndian.PutUint64(st.Bytes[64*i+8:], ste1)
	}
	write64(r, STRTAB_BASE, st.Phys|1<<62 /*RA*/)
	r.Write32(STRTAB_BASE_CFG, uint32(log2SIDs)) // FMT linear

	// Queues.
	if s.cmdq, err = alloc(16<<queueLog2, 16<<queueLog2); err != nil {
		return nil, err
	}
	if s.evq, err = alloc(32<<queueLog2, 32<<queueLog2); err != nil {
		return nil, err
	}
	write64(r, CMDQ_BASE, s.cmdq.Phys|queueLog2|1<<62)
	r.Write32(CMDQ_PROD, 0)
	r.Write32(CMDQ_CONS, 0)
	write64(r, EVENTQ_BASE, s.evq.Phys|queueLog2|1<<62)
	r.Write32(EVENTQ_PROD, 0)
	r.Write32(EVENTQ_CONS, 0)
	r.Write32(CR1, 0) // table and queue walks: non-cacheable, matches QEMU and any memory type
	r.Write32(CR2, 0)

	r.Write32(CR0, CR0_CMDQEN|CR0_EVENTQEN)
	if err := s.waitAck(CR0_CMDQEN | CR0_EVENTQEN); err != nil {
		return nil, err
	}
	if err := s.commands([2]uint64{cmdCfgiAll, 31}, [2]uint64{cmdTlbiNsnh, 0}); err != nil {
		return nil, err
	}
	r.Write32(CR0, CR0_CMDQEN|CR0_EVENTQEN|CR0_SMMUEN)
	if err := s.waitAck(CR0_CMDQEN | CR0_EVENTQEN | CR0_SMMUEN); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SMMU) waitAck(want uint32) error {
	deadline := time.Now().Add(100 * time.Millisecond)
	for s.r.Read32(CR0ACK) != want {
		if time.Now().After(deadline) {
			return fmt.Errorf("smmuv3: CR0ACK %#x, want %#x", s.r.Read32(CR0ACK), want)
		}
		time.Sleep(10 * time.Microsecond)
	}
	return nil
}

// commands queues commands and a CMD_SYNC, and waits for the SMMU to
// consume them.
func (s *SMMU) commands(cmds ...[2]uint64) error {
	n := uint32(1) << queueLog2
	put := func(c [2]uint64) {
		i := s.cmdProd & (n - 1)
		binary.LittleEndian.PutUint64(s.cmdq.Bytes[16*i:], c[0])
		binary.LittleEndian.PutUint64(s.cmdq.Bytes[16*i+8:], c[1])
		s.cmdProd = (s.cmdProd + 1) & (2*n - 1) // index plus wrap bit
	}
	for _, c := range cmds {
		put(c)
	}
	put([2]uint64{cmdSync, 0})
	s.r.Write32(CMDQ_PROD, s.cmdProd)
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		cons := s.r.Read32(CMDQ_CONS)
		if cons>>24&0x7f != 0 { // ERR field
			return fmt.Errorf("smmuv3: command error %#x", cons>>24&0x7f)
		}
		if cons&(2*n-1) == s.cmdProd {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("smmuv3: commands not consumed (prod %#x cons %#x)", s.cmdProd, cons)
		}
		time.Sleep(10 * time.Microsecond)
	}
}

// Events drains the event queue: translation faults, each with its stream
// and address.
func (s *SMMU) Events() []string {
	n := uint32(1) << queueLog2
	for i := 0; i < 64; i++ {
		prod, cons := s.r.Read32(EVENTQ_PROD), s.r.Read32(EVENTQ_CONS)
		if prod&(2*n-1) == cons&(2*n-1) {
			break
		}
		e := s.evq.Bytes[32*(cons&(n-1)):]
		id := binary.LittleEndian.Uint32(e) & 0xff
		sid := binary.LittleEndian.Uint32(e[4:])
		addr := binary.LittleEndian.Uint64(e[16:])
		if len(s.faults) < 16 {
			s.faults = append(s.faults, fmt.Sprintf("event %#x stream %#x address %#x", id, sid, addr))
		}
		s.r.Write32(EVENTQ_CONS, (cons+1)&(2*n-1))
	}
	return s.faults
}

// Info describes the mapping for lsdev.
func (s *SMMU) Info() string {
	return fmt.Sprintf("SMMUv3: DMA region at bus %#x+%#x for %d streams", s.bus, s.size, s.sids)
}
