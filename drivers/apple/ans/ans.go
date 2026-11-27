// Package ans drives the M1 Pro's internal SSD: Apple's ANS2 coprocessor,
// an NVMe controller with Apple changes, after m1n1's nvme.c (and Linux's
// nvme/host/apple.c). The ANS firmware runs under RTKit (mailbox: package
// asc) behind a SART (package sart); once it reports booted, the NVMe side
// is standard except for a linear submission queue (the doorbell takes the
// command's slot, not a tail) and the NVMMU, which wants a TCB per slot
// repeating the command's DMA addresses.
//
// One goroutine owns the controller; one command is in flight at a time
// (slot 0). The disk is read-only unless Config.Writes is set, and even
// then it is fenced (block.Fence): writes land only in the FAT32 partition
// labelled GODEEPDATA. The SSD holds macOS and Omarchy.
package ans

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"godeep/drivers/apple/asc"
	"godeep/drivers/apple/pmgr"
	"godeep/drivers/apple/sart"
	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
	"godeep/kernel/rtkit"
)

// Compatible is the SSD controller in the M1 Pro's device tree.
const Compatible = "apple,t6000-nvme-ans2"

// NVMe and ANS registers (the "nvme" window).
const (
	CC            = 0x14
	CSTS          = 0x1c
	AQA           = 0x24
	ASQ           = 0x28
	ACQ           = 0x30
	DB_ACQ        = 0x1004
	DB_IOCQ       = 0x100c
	MAX_PEND_CMDS = 0x1210
	BOOT_STATUS   = 0x1300
	LINEAR_SQ_CTL = 0x24908
	DB_LINEAR_ASQ = 0x2490c
	DB_LINEAR_IOQ = 0x24910
	NVMMU_NUM     = 0x28100
	NVMMU_ASQ     = 0x28108
	NVMMU_IOSQ    = 0x28110
	NVMMU_INVAL   = 0x28118
	NVMMU_STAT    = 0x28120

	CC_EN          = 1 << 0
	CC_SHN_MASK    = 3 << 14
	CC_SHN_NORMAL  = 1 << 14
	CSTS_RDY       = 1 << 0
	CSTS_SHST_MASK = 3 << 2
	CSTS_SHST_DONE = 2 << 2
	BootOK         = 0xde71ce55

	QueueSize = 64
	PageSize  = 4096
	MaxPages  = 16 // per command: 64 KB

	opCreateCQ = 0x05
	opCreateSQ = 0x01
	opDeleteSQ = 0x00
	opDeleteCQ = 0x04
	opIdentify = 0x06
	opRead     = 0x02
	opWrite    = 0x01

	tcbToDevice   = 1 << 1
	tcbFromDevice = 1 << 0

	BootTimeout = 5 * time.Second
	CmdTimeout  = 2 * time.Second
)

// Config is what the board resolves from the device tree.
type Config struct {
	NVMe, ANS, Mailbox, SART uint64
	Power                    []pmgr.Domain // parents first
	Reset                    *pmgr.Domain  // ps_ans2: reset before every boot
	MailboxIRQs              []int         // the mailbox's interrupts (AIC ids), if the board has AIC
	// Writes turns on writing, always fenced to the GODEEPDATA partition.
	// Off until reads have been confirmed on the Mac.
	Writes bool
}

// Fence is the write policy for the SSD: godeep's own FAT32 partition.
var Fence = block.BasicDataFAT32("GODEEPDATA")

// Driver returns the SSD driver.
func Driver(regs func(base uint64) mmio.Regs64) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		switch c := d.Config.(type) {
		case Config:
			return run(ctx, d, regs, c)
		case error:
			return c
		}
		return errors.New("ans: no configuration from the board")
	}}
}

// Disk is the SSD: a read-only block.Device.
type Disk struct {
	reqs     chan *req
	down     chan struct{}
	bs       int
	blocks   uint64
	info     string
	reads    atomic.Uint64
	writes   atomic.Uint64
	writable bool
}

type req struct {
	lba   uint64
	p     []byte
	write bool
	done  chan error
}

var _ block.Device = (*Disk)(nil)

func (k *Disk) BlockSize() int { return k.bs }
func (k *Disk) Blocks() uint64 { return k.blocks }
func (k *Disk) Devices() []string {
	mode := "read-only"
	if k.writable {
		mode = fmt.Sprintf("%d writes", k.writes.Load())
	}
	return []string{fmt.Sprintf("%s, %d reads, %s", k.info, k.reads.Load(), mode)}
}

// writer is the Disk with writes: only ever handed to block.Fence.
type writer struct{ *Disk }

func (w writer) WriteBlocks(ctx context.Context, lba uint64, p []byte) error {
	return w.do(ctx, lba, p, true)
}

func (k *Disk) ReadBlocks(ctx context.Context, lba uint64, p []byte) error {
	return k.do(ctx, lba, p, false)
}

func (k *Disk) do(ctx context.Context, lba uint64, p []byte, write bool) error {
	if len(p)%k.bs != 0 {
		return fmt.Errorf("ans: transfer of %d bytes", len(p))
	}
	if lba+uint64(len(p)/k.bs) > k.blocks {
		return block.ErrRange
	}
	r := &req{lba: lba, p: p, write: write, done: make(chan error, 1)}
	select {
	case k.reqs <- r:
	case <-k.down:
		return errors.New("ans: controller stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-r.done
}

// queue is a submission/completion queue pair with its TCBs.
type queue struct {
	cmds, cqes, tcbs *dma.Buffer
	head             int
	phase            uint32
	db               uint64 // the linear submission doorbell
	cqdb             uint64
}

type ctl struct {
	r          mmio.Regs64
	admin, io  *queue
	prps, data *dma.Buffer
}

func newQueue(a *dma.Allocator, db, cqdb uint64) (*queue, error) {
	q := &queue{phase: 1, db: db, cqdb: cqdb}
	var err error
	for _, p := range []**dma.Buffer{&q.cmds, &q.cqes, &q.tcbs} {
		if *p, err = a.Alloc(QueueSize*128, 16384); err != nil {
			return nil, err
		}
	}
	return q, nil
}

func (q *queue) free() {
	for _, b := range []*dma.Buffer{q.cmds, q.cqes, q.tcbs} {
		if b != nil {
			b.Free()
		}
	}
}

func run(ctx context.Context, d *dev.Device, regs func(uint64) mmio.Regs64, c Config) error {
	if d.DMA == nil {
		return errors.New("ans: the board has no DMA region")
	}
	for _, p := range c.Power {
		if _, err := pmgr.On(ctx, regs(p.Addr)); err != nil {
			return fmt.Errorf("ans: power domain %s: %w", p.Name, err)
		}
	}
	// The firmware may reach godeep's whole DMA region (RTKit's buffers).
	s := sart.New(regs(c.SART))
	base, size := d.DMA.Region()
	if err := s.Allow(base, size&^(PageSize-1)); err != nil {
		return err
	}
	defer s.Clear()

	// As Linux's apple_nvme_reset_work: stop the CPU, reset the block,
	// start it again and boot RTKit from scratch, whatever state iBoot or
	// m1n1 left the firmware in.
	cpu := regs(c.ANS)
	note := ""
	if asc.CPURunning(cpu) {
		note = "; found running, restarted"
	}
	asc.StopCPU(cpu)
	if c.Reset != nil {
		if err := pmgr.Reset(ctx, regs(c.Reset.Addr)); err != nil {
			return fmt.Errorf("ans: reset: %w", err)
		}
	}

	// The mailbox and RTKit outlive ctx: when the driver is stopped (halt),
	// the firmware still has to be told to shut down.
	mctx, stopMbox := context.WithCancel(context.WithoutCancel(ctx))
	defer stopMbox()
	mb := asc.New(regs(c.Mailbox))
	line := asc.RecvLine(d.Bridge, c.MailboxIRQs, "ans mailbox")
	d.Proc.Go(func(context.Context) { mb.Run(mctx, line) })
	rk := rtkit.New("ans", mb, d.DMA, sart.IOMMU{})
	d.Proc.Go(func(context.Context) { rk.Run(mctx) })
	asc.StartCPU(cpu)
	bctx, cancel := context.WithTimeout(ctx, BootTimeout)
	err := rk.Boot(bctx)
	cancel()
	if err != nil {
		return fmt.Errorf("ans: firmware boot: %w (%s)", err, mb.Diag())
	}

	r := regs(c.NVMe)
	if err := poll(ctx, func() bool { return r.Read32(BOOT_STATUS) == BootOK }, BootTimeout); err != nil {
		return fmt.Errorf("ans: boot status %#x", r.Read32(BOOT_STATUS))
	}
	k, cl, err := setup(ctx, r, d.DMA)
	if cl != nil {
		defer cl.free()
	}
	if err != nil {
		return err
	}
	k.info = fmt.Sprintf("%s; RTKit v%d%s", k.info, rk.Info().Version, note)
	defer close(k.down)
	var fenced chan *block.Fenced
	if c.Writes {
		// The fence reads the partition table through the disk: serve
		// requests while it does, then publish the fenced disk.
		k.writable = true
		fenced = make(chan *block.Fenced, 1)
		d.Proc.Go(func(ctx context.Context) {
			f, err := block.Fence(ctx, writer{k}, Fence)
			if err != nil {
				f = nil
			}
			fenced <- f
		})
	} else {
		d.Ready(k)
	}
	for {
		select {
		case <-ctx.Done():
			cl.shutdown()
			sctx, cancel := context.WithTimeout(context.Background(), time.Second)
			rk.Shutdown(sctx)
			cancel()
			return nil
		case f := <-fenced:
			fenced = nil
			if f == nil {
				return errors.New("ans: can't read the partition table to fence writes")
			}
			d.Ready(f)
		case q := <-k.reqs:
			var err error
			if q.write {
				err = cl.write(ctx, k, q.lba, q.p)
				k.writes.Add(1)
			} else {
				err = cl.read(ctx, k, q.lba, q.p)
				k.reads.Add(1)
			}
			q.done <- err
		}
	}
}

func poll(ctx context.Context, ok func() bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			return errors.New("timeout")
		}
		t := time.NewTimer(100 * time.Microsecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

func write64(r mmio.Regs, off, v uint64) { // low word first, as m1n1 does
	r.Write32(off, uint32(v))
	r.Write32(off+4, uint32(v>>32))
}

// setup enables the controller with linear submission and the NVMMU,
// creates the I/O queues and identifies namespace 1.
func setup(ctx context.Context, r mmio.Regs64, a *dma.Allocator) (*Disk, *ctl, error) {
	c := &ctl{r: r}
	var err error
	if c.admin, err = newQueue(a, DB_LINEAR_ASQ, DB_ACQ); err != nil {
		return nil, c, err
	}
	if c.io, err = newQueue(a, DB_LINEAR_IOQ, DB_IOCQ); err != nil {
		return nil, c, err
	}
	if c.prps, err = a.Alloc(PageSize, PageSize); err != nil {
		return nil, c, err
	}
	if c.data, err = a.Alloc(MaxPages*PageSize, 16384); err != nil {
		return nil, c, err
	}
	r.Write32(LINEAR_SQ_CTL, r.Read32(LINEAR_SQ_CTL)|1)
	r.Write32(MAX_PEND_CMDS, (QueueSize-1)<<16|(QueueSize-1))
	r.Write32(NVMMU_NUM, QueueSize-1)
	write64(r, NVMMU_ASQ, c.admin.tcbs.Phys)
	write64(r, NVMMU_IOSQ, c.io.tcbs.Phys)

	r.Write32(CC, r.Read32(CC)&^CC_EN)
	if err := poll(ctx, func() bool { return r.Read32(CSTS)&CSTS_RDY == 0 }, time.Second); err != nil {
		return nil, c, errors.New("ans: controller won't disable")
	}
	write64(r, ASQ, c.admin.cmds.Phys)
	write64(r, ACQ, c.admin.cqes.Phys)
	r.Write32(AQA, (QueueSize-1)<<16|(QueueSize-1))
	r.Write32(CC, r.Read32(CC)&^CC_SHN_MASK|CC_EN)
	if err := poll(ctx, func() bool { return r.Read32(CSTS)&CSTS_RDY != 0 }, 5*time.Second); err != nil {
		return nil, c, errors.New("ans: controller won't enable")
	}
	if _, err := c.exec(ctx, c.admin, cmd{op: opCreateCQ, prp1: c.io.cqes.Phys, cdw10: 1 | (QueueSize-1)<<16, cdw11: 1}); err != nil {
		return nil, c, fmt.Errorf("ans: create I/O CQ: %w", err)
	}
	if _, err := c.exec(ctx, c.admin, cmd{op: opCreateSQ, prp1: c.io.cmds.Phys, cdw10: 1 | (QueueSize-1)<<16, cdw11: 1 | 1<<16}); err != nil {
		return nil, c, fmt.Errorf("ans: create I/O SQ: %w", err)
	}

	k := &Disk{reqs: make(chan *req), down: make(chan struct{})}
	if _, err := c.exec(ctx, c.admin, cmd{op: opIdentify, prp1: c.data.Phys, cdw10: 1}); err != nil {
		return nil, c, fmt.Errorf("ans: identify controller: %w", err)
	}
	id := c.data.Bytes
	model := trim(id[24:64])
	fw := trim(id[64:72])
	if _, err := c.exec(ctx, c.admin, cmd{op: opIdentify, nsid: 1, prp1: c.data.Phys, cdw10: 0}); err != nil {
		return nil, c, fmt.Errorf("ans: identify namespace: %w", err)
	}
	k.blocks = binary.LittleEndian.Uint64(id[0:])
	lbaf := int(id[26] & 0xf)
	k.bs = 1 << (binary.LittleEndian.Uint32(id[128+4*lbaf:]) >> 16 & 0xff)
	if k.bs < 512 || k.bs > PageSize || k.blocks == 0 {
		return nil, c, fmt.Errorf("ans: namespace 1: %d blocks of %d bytes", k.blocks, k.bs)
	}
	k.info = fmt.Sprintf("%s (firmware %s), namespace 1: %d GB, %d-byte blocks", model, fw,
		k.blocks*uint64(k.bs)/1e9, k.bs)
	return k, c, nil
}

func trim(b []byte) string {
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == 0) {
		b = b[:len(b)-1]
	}
	return string(b)
}

type cmd struct {
	op                         byte
	nsid                       uint32
	prp1, prp2                 uint64
	cdw10, cdw11, cdw12, cdw13 uint32
}

// exec runs one command in slot 0 and waits for its completion.
func (c *ctl) exec(ctx context.Context, q *queue, x cmd) (uint32, error) {
	const tag = 0
	sq := q.cmds.Bytes[tag*64 : tag*64+64]
	clear(sq)
	sq[0] = x.op
	sq[2] = tag
	le := binary.LittleEndian
	le.PutUint32(sq[4:], x.nsid)
	le.PutUint64(sq[24:], x.prp1)
	le.PutUint64(sq[32:], x.prp2)
	le.PutUint32(sq[40:], x.cdw10)
	le.PutUint32(sq[44:], x.cdw11)
	le.PutUint32(sq[48:], x.cdw12)
	le.PutUint32(sq[52:], x.cdw13)

	tcb := q.tcbs.Bytes[tag*128 : tag*128+128]
	clear(tcb)
	switch {
	case x.prp1 == 0:
	case x.op&1 != 0:
		tcb[1] = tcbToDevice
	default:
		tcb[1] = tcbFromDevice
	}
	tcb[2] = tag
	le.PutUint32(tcb[4:], x.cdw12)
	le.PutUint64(tcb[24:], x.prp1)
	le.PutUint64(tcb[32:], x.prp2)

	c.r.Write32(q.db, tag) // release: the command and TCB are visible first
	off := q.head * 16
	var status uint32
	err := poll(ctx, func() bool {
		status = q.cqes.Load32(off+12) >> 16
		return status&1 == q.phase
	}, CmdTimeout)
	if err != nil {
		return 0, fmt.Errorf("command %#x: no completion", x.op)
	}
	result := q.cqes.Load32(off)
	got := int(q.cqes.Load32(off+12) & 0xffff)
	c.r.Write32(NVMMU_INVAL, uint32(got))
	stat := c.r.Read32(NVMMU_STAT)
	if q.head++; q.head == QueueSize {
		q.head, q.phase = 0, q.phase^1
	}
	c.r.Write32(q.cqdb, uint32(q.head))
	if got != tag {
		return 0, fmt.Errorf("command %#x: completion for tag %d", x.op, got)
	}
	if status>>1 != 0 {
		return 0, fmt.Errorf("command %#x failed: status %#x", x.op, status>>1)
	}
	if stat != 0 {
		return 0, fmt.Errorf("command %#x: NVMMU invalidation failed (%#x)", x.op, stat)
	}
	return result, nil
}

// read fills p from lba on, MaxPages at a time.
func (c *ctl) read(ctx context.Context, k *Disk, lba uint64, p []byte) error {
	return c.rw(ctx, k, lba, p, false)
}

// write writes p from lba on (only ever reached through the fence).
func (c *ctl) write(ctx context.Context, k *Disk, lba uint64, p []byte) error {
	return c.rw(ctx, k, lba, p, true)
}

func (c *ctl) rw(ctx context.Context, k *Disk, lba uint64, p []byte, write bool) error {
	op := byte(opRead)
	if write {
		op = opWrite
	}
	for len(p) > 0 {
		n := min(len(p), MaxPages*PageSize)
		pages := (n + PageSize - 1) / PageSize
		x := cmd{op: op, nsid: 1, prp1: c.data.Phys, cdw10: uint32(lba), cdw11: uint32(lba >> 32),
			cdw12: uint32(n/k.bs - 1)}
		switch {
		case pages == 2:
			x.prp2 = c.data.Phys + PageSize
		case pages > 2:
			for i := 1; i < pages; i++ {
				binary.LittleEndian.PutUint64(c.prps.Bytes[8*(i-1):], c.data.Phys+uint64(i*PageSize))
			}
			x.prp2 = c.prps.Phys
		}
		if write {
			copy(c.data.Bytes[:n], p[:n])
		}
		if _, err := c.exec(ctx, c.io, x); err != nil {
			return err
		}
		if !write {
			copy(p, c.data.Bytes[:n])
		}
		p, lba = p[n:], lba+uint64(n/k.bs)
	}
	return nil
}

// shutdown deletes the I/O queues and shuts the controller down cleanly.
func (c *ctl) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c.exec(ctx, c.admin, cmd{op: opDeleteSQ, cdw10: 1})
	c.exec(ctx, c.admin, cmd{op: opDeleteCQ, cdw10: 1})
	c.r.Write32(CC, c.r.Read32(CC)&^CC_SHN_MASK|CC_SHN_NORMAL)
	poll(ctx, func() bool { return c.r.Read32(CSTS)&CSTS_SHST_MASK == CSTS_SHST_DONE }, 2*time.Second)
	c.r.Write32(CC, c.r.Read32(CC)&^CC_EN)
}

func (c *ctl) free() {
	for _, q := range []*queue{c.admin, c.io} {
		if q != nil {
			q.free()
		}
	}
	for _, b := range []*dma.Buffer{c.prps, c.data} {
		if b != nil {
			b.Free()
		}
	}
}
