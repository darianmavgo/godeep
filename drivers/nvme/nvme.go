// Package nvme drives an NVM Express controller over PCIe (class 010802),
// polled, reads and writes: it brings the controller up, identifies it and its
// first namespace, creates one I/O queue pair and serves block reads. The
// published service is a block.Device.
//
// The command layer (queues, identify, read, PRP lists) is the part Apple's
// SSD controller (ANS2) shares; on the Mac it is reached through RTKit and
// SART instead of plain PCIe, with Apple's changes to submission.
//
// One goroutine (the driver's) owns the controller. ReadBlocks callers hand
// it requests over a channel, so concurrent readers are serialized.
package nvme

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"godeep/drivers/pci"
	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

// Compatible is the PCI class string for NVMe.
const Compatible = "pciclass,010802"

// Controller registers.
const (
	CAP   = 0x00 // 64-bit
	VS    = 0x08
	INTMS = 0x0c
	CC    = 0x14
	CSTS  = 0x1c
	AQA   = 0x24
	ASQ   = 0x28 // 64-bit
	ACQ   = 0x30 // 64-bit
	DB    = 0x1000

	CC_EN     = 1 << 0
	CC_SHN_NM = 1 << 14 // normal shutdown
	CC_IOSQES = 6 << 16 // 64-byte submission entries
	CC_IOCQES = 4 << 20 // 16-byte completion entries

	CSTS_RDY  = 1 << 0
	CSTS_CFS  = 1 << 1
	CSTS_SHST = 3 << 2
)

// Commands.
const (
	opCreateSQ = 0x01
	opCreateCQ = 0x05
	opIdentify = 0x06
	opRead     = 0x02
	opWrite    = 0x01
)

const (
	pageSize    = 4096
	queueDepth  = 32
	sqeSize     = 64
	cqeSize     = 16
	maxTransfer = 128 << 10 // bytes per command (less if the controller says so)
	cmdTimeout  = 5 * time.Second
)

// Driver returns the NVMe driver. The node's Config is its *pci.Function
// and Reg[0] its BAR0.
//
// fence, if given, decides which disks are fenced (by serial number) and
// how: their writes are confined to the partitions the policy allows
// (block.Fence). QEMU virt uses it to rehearse the Mac SSD's fence.
func Driver(regs func(base uint64) mmio.Regs, fence ...func(serial string) block.Policy) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		f, ok := d.Config.(*pci.Function)
		if !ok || f.BARs[0].Size == 0 {
			return errors.New("nvme: no PCI function with a BAR0")
		}
		if d.DMA == nil {
			return errors.New("nvme: the board has no DMA region")
		}
		a := d.DMA
		if f.DMA != nil { // behind an IOMMU: buffers' Bus addresses are what it sees
			a = f.DMA
		}
		c := &ctrl{r: regs(f.BARs[0].CPU), dma: a}
		if err := c.init(ctx); err != nil {
			c.free()
			return err
		}
		defer c.free()
		defer c.shutdown()
		disk := &Disk{reqs: make(chan *req), info: c.info, bs: c.bs, blocks: c.blocks}
		var policy block.Policy
		for _, f := range fence {
			if p := f(c.serial); p != nil {
				policy = p
			}
		}
		if policy == nil {
			d.Ready(disk)
		} else {
			// The fence reads the partition table through the disk, so
			// serve requests while it does.
			fenced := make(chan *block.Fenced, 1)
			fctx, cancel := context.WithCancel(ctx)
			d.Proc.Go(func(context.Context) { // ends with fctx; touches only the disk's request channel
				f, err := block.Fence(fctx, disk, policy)
				if err != nil {
					f = nil
				}
				fenced <- f
			})
		wait:
			for {
				select {
				case f := <-fenced:
					cancel()
					if f == nil {
						return errors.New("nvme: can't read the partition table to fence the disk")
					}
					d.Ready(f)
					break wait
				case r := <-disk.reqs:
					r.done <- c.read(ctx, r.lba, r.p) // the fence only reads
				case <-ctx.Done():
					cancel()
					return nil
				}
			}
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case r := <-disk.reqs:
				if r.write {
					r.done <- c.write(ctx, r.lba, r.p)
				} else {
					r.done <- c.read(ctx, r.lba, r.p)
				}
			}
		}
	}}
}

// Disk is the published service: namespace 1 as a block device.
type Disk struct {
	reqs   chan *req
	info   string
	bs     int
	blocks uint64
}

type req struct {
	lba   uint64
	p     []byte
	write bool
	done  chan error
}

func (d *Disk) BlockSize() int    { return d.bs }
func (d *Disk) Blocks() uint64    { return d.blocks }
func (d *Disk) Devices() []string { return []string{d.info} }
func (d *Disk) ReadBlocks(ctx context.Context, lba uint64, p []byte) error {
	return d.do(ctx, lba, p, false)
}

// WriteBlocks implements block.Writer.
func (d *Disk) WriteBlocks(ctx context.Context, lba uint64, p []byte) error {
	return d.do(ctx, lba, p, true)
}

func (d *Disk) do(ctx context.Context, lba uint64, p []byte, write bool) error {
	if err := block.Check(d, lba, p); err != nil {
		return err
	}
	r := &req{lba: lba, p: p, write: write, done: make(chan error, 1)}
	select {
	case d.reqs <- r:
	case <-ctx.Done():
		return ctx.Err()
	}
	// Accepted: the driver always answers (it copies into p, so this must
	// not return before it has).
	return <-r.done
}

// queue is a submission/completion queue pair.
type queue struct {
	id      int
	sq, cq  *dma.Buffer
	tail    int
	head    int
	phase   uint32
	sqDB    uint64
	cqDB    uint64
	nextCID uint16
}

type ctrl struct {
	serial  string
	r       mmio.Regs
	dma     *dma.Allocator
	dstrd   uint64
	admin   *queue
	io      *queue
	buf     *dma.Buffer // data bounce buffer
	prps    *dma.Buffer // PRP list page
	maxXfer int
	bs      int
	blocks  uint64
	info    string
}

func (c *ctrl) read64(off uint64) uint64 {
	return uint64(c.r.Read32(off)) | uint64(c.r.Read32(off+4))<<32
}

func (c *ctrl) write64(off, v uint64) {
	c.r.Write32(off, uint32(v))
	c.r.Write32(off+4, uint32(v>>32))
}

func (c *ctrl) waitReady(ctx context.Context, want bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st := c.r.Read32(CSTS)
		if st&CSTS_CFS != 0 {
			return errors.New("nvme: controller fatal status")
		}
		if (st&CSTS_RDY != 0) == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nvme: CSTS.RDY didn't become %v within %v", want, timeout)
		}
		if err := sleep(ctx, time.Millisecond); err != nil {
			return err
		}
	}
}

func (c *ctrl) newQueue(id int) (*queue, error) {
	sq, err := c.dma.Alloc(queueDepth*sqeSize, pageSize)
	if err != nil {
		return nil, err
	}
	cq, err := c.dma.Alloc(queueDepth*cqeSize, pageSize)
	if err != nil {
		sq.Free()
		return nil, err
	}
	stride := uint64(4) << c.dstrd
	return &queue{id: id, sq: sq, cq: cq, phase: 1,
		sqDB: DB + uint64(2*id)*stride, cqDB: DB + uint64(2*id+1)*stride}, nil
}

func (c *ctrl) init(ctx context.Context) error {
	caps := c.read64(CAP)
	mqes := int(caps&0xffff) + 1
	timeout := time.Duration(caps>>24&0xff) * 500 * time.Millisecond
	if timeout == 0 {
		timeout = 500 * time.Millisecond
	}
	c.dstrd = caps >> 32 & 0xf
	if mpsmin := caps >> 48 & 0xf; mpsmin != 0 {
		return fmt.Errorf("nvme: minimum page size %d KB; only 4 KB is supported", 4<<mpsmin)
	}
	if caps>>37&1 == 0 {
		return errors.New("nvme: controller lacks the NVM command set")
	}
	if mqes < queueDepth {
		return fmt.Errorf("nvme: queues limited to %d entries", mqes)
	}
	if c.r.Read32(CC)&CC_EN != 0 { // left enabled (by firmware or a previous run)
		c.r.Write32(CC, 0)
		if err := c.waitReady(ctx, false, timeout); err != nil {
			return err
		}
	}

	var err error
	if c.admin, err = c.newQueue(0); err != nil {
		return err
	}
	c.r.Write32(INTMS, 0xffffffff) // polled: no interrupts
	c.r.Write32(AQA, (queueDepth-1)<<16|(queueDepth-1))
	c.write64(ASQ, c.admin.sq.Bus)
	c.write64(ACQ, c.admin.cq.Bus)
	c.r.Write32(CC, CC_IOCQES|CC_IOSQES|CC_EN)
	if err := c.waitReady(ctx, true, timeout); err != nil {
		return err
	}

	if c.buf, err = c.dma.Alloc(maxTransfer, pageSize); err != nil {
		return err
	}
	if c.prps, err = c.dma.Alloc(pageSize, pageSize); err != nil {
		return err
	}

	// Identify controller.
	if err := c.cmd(ctx, c.admin, opIdentify, 0, c.buf.Bus, 0, [6]uint32{1}); err != nil {
		return fmt.Errorf("nvme: identify controller: %w", err)
	}
	id := c.buf.Bytes[:4096]
	model := strings.TrimSpace(string(id[24:64]))
	serial := strings.TrimSpace(string(id[4:24]))
	c.serial = serial
	c.maxXfer = maxTransfer
	if mdts := id[77]; mdts != 0 && pageSize<<mdts < c.maxXfer {
		c.maxXfer = pageSize << mdts
	}
	nn := binary.LittleEndian.Uint32(id[516:])
	if nn == 0 {
		return errors.New("nvme: no namespaces")
	}

	// Identify namespace 1.
	if err := c.cmd(ctx, c.admin, opIdentify, 1, c.buf.Bus, 0, [6]uint32{0}); err != nil {
		return fmt.Errorf("nvme: identify namespace: %w", err)
	}
	ns := c.buf.Bytes[:4096]
	c.blocks = binary.LittleEndian.Uint64(ns[0:])
	lbaf := binary.LittleEndian.Uint32(ns[128+4*int(ns[26]&0xf):])
	c.bs = 1 << (lbaf >> 16 & 0xff)
	if c.blocks == 0 || c.bs < 512 || c.bs > pageSize || lbaf&0xffff != 0 {
		return fmt.Errorf("nvme: namespace 1: %d blocks of %d bytes, %d bytes metadata", c.blocks, c.bs, lbaf&0xffff)
	}

	// One I/O queue pair: completion queue first.
	if c.io, err = c.newQueue(1); err != nil {
		return err
	}
	if err := c.cmd(ctx, c.admin, opCreateCQ, 0, c.io.cq.Bus, 0, [6]uint32{(queueDepth-1)<<16 | 1, 1}); err != nil {
		return fmt.Errorf("nvme: create I/O completion queue: %w", err)
	}
	if err := c.cmd(ctx, c.admin, opCreateSQ, 0, c.io.sq.Bus, 0, [6]uint32{(queueDepth-1)<<16 | 1, 1<<16 | 1}); err != nil {
		return fmt.Errorf("nvme: create I/O submission queue: %w", err)
	}
	vs := c.r.Read32(VS)
	c.info = fmt.Sprintf("%s (serial %s), NVMe %d.%d, namespace 1: %d MiB, %d-byte blocks",
		model, serial, vs>>16, vs>>8&0xff, c.blocks*uint64(c.bs)>>20, c.bs)
	return nil
}

// cmd submits one command and polls for its completion.
func (c *ctrl) cmd(ctx context.Context, q *queue, op byte, nsid uint32, prp1, prp2 uint64, cdw [6]uint32) error {
	cid := q.nextCID
	q.nextCID++
	e := q.sq.Bytes[q.tail*sqeSize : (q.tail+1)*sqeSize]
	clear(e)
	le := binary.LittleEndian
	le.PutUint32(e[4:], nsid)
	le.PutUint64(e[24:], prp1)
	le.PutUint64(e[32:], prp2)
	for i, v := range cdw {
		le.PutUint32(e[40+4*i:], v)
	}
	q.sq.Store32(q.tail*sqeSize, uint32(cid)<<16|uint32(op)) // release: the entry is complete
	q.tail = (q.tail + 1) % queueDepth
	c.r.Write32(q.sqDB, uint32(q.tail))

	deadline := time.Now().Add(cmdTimeout)
	for spins := 0; ; spins++ {
		dw3 := q.cq.Load32(q.head*cqeSize + 12) // acquire
		if dw3>>16&1 == q.phase {
			q.head++
			if q.head == queueDepth {
				q.head, q.phase = 0, q.phase^1
			}
			c.r.Write32(q.cqDB, uint32(q.head))
			if got := uint16(dw3); got != cid {
				return fmt.Errorf("nvme: completion for command %d, expected %d", got, cid)
			}
			if st := dw3 >> 17 & 0x7fff; st != 0 {
				return fmt.Errorf("nvme: status type %d code %#x", st>>8&7, st&0xff)
			}
			return nil
		}
		if spins < 1000 {
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nvme: command %#x timed out", op)
		}
		if err := sleep(ctx, 50*time.Microsecond); err != nil {
			return err
		}
	}
}

// read fills p from lba, in transfers of at most maxXfer bytes.
func (c *ctrl) read(ctx context.Context, lba uint64, p []byte) error {
	return c.transfer(ctx, opRead, lba, p)
}

// write writes p at lba.
func (c *ctrl) write(ctx context.Context, lba uint64, p []byte) error {
	return c.transfer(ctx, opWrite, lba, p)
}

// transfer runs reads or writes through the bounce buffer.
func (c *ctrl) transfer(ctx context.Context, op byte, lba uint64, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), c.maxXfer)
		nlb := n / c.bs
		pages := (n + pageSize - 1) / pageSize
		prp1, prp2 := c.buf.Bus, uint64(0)
		switch {
		case pages == 2:
			prp2 = c.buf.Bus + pageSize
		case pages > 2:
			for i := 1; i < pages; i++ {
				binary.LittleEndian.PutUint64(c.prps.Bytes[8*(i-1):], c.buf.Bus+uint64(i)*pageSize)
			}
			prp2 = c.prps.Bus
		}
		if op == opWrite {
			copy(c.buf.Bytes[:n], p)
		}
		if err := c.cmd(ctx, c.io, op, 1, prp1, prp2, [6]uint32{uint32(lba), uint32(lba >> 32), uint32(nlb - 1)}); err != nil {
			return fmt.Errorf("nvme: %s %d blocks at %d: %w", map[byte]string{opRead: "read", opWrite: "write"}[op], nlb, lba, err)
		}
		if op == opRead {
			copy(p, c.buf.Bytes[:n])
		}
		p, lba = p[n:], lba+uint64(nlb)
	}
	return nil
}

// shutdown tells the controller to flush and stop (bounded wait).
func (c *ctrl) shutdown() {
	c.r.Write32(CC, c.r.Read32(CC)|CC_SHN_NM)
	for i := 0; i < 100 && c.r.Read32(CSTS)&CSTS_SHST != 2<<2; i++ {
		time.Sleep(time.Millisecond)
	}
}

func (c *ctrl) free() {
	for _, q := range []*queue{c.admin, c.io} {
		if q != nil {
			q.sq.Free()
			q.cq.Free()
		}
	}
	for _, b := range []*dma.Buffer{c.buf, c.prps} {
		if b != nil {
			b.Free()
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
