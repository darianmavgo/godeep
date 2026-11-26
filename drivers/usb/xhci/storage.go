package xhci

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"

	"godeep/drivers/usb"
	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
)

// USB mass storage, Bulk-Only Transport (BOT) with SCSI commands:
// INQUIRY, TEST UNIT READY, READ CAPACITY(10), READ(10), WRITE(10), and
// REQUEST SENSE after a failure. A stalled endpoint is recovered (clear
// the device's halt, reset the xHCI endpoint, move its dequeue pointer
// past the failed transfer), and a "unit attention" is retried once. Each
// disk is published as its own device ("usbdisk0", compatible
// DiskCompatible) whose service is a block.Device, so lsblk, partitions and
// the FAT/ext4 readers see it like an NVMe disk. Reads are carried out by
// the controller's goroutine (it owns every ring).

// DiskCompatible is the device-manager entry for a USB disk; boards
// register DiskDriver for it.
const DiskCompatible = "godeep,usb-storage"

const (
	cbwSig      = 0x43425355 // "USBC"
	cswSig      = 0x53425355 // "USBS"
	maxXfer     = 64 << 10   // bytes per READ(10)
	scsiTUR     = 0x00
	scsiInq     = 0x12
	scsiCap10   = 0x25
	scsiRead10  = 0x28
	scsiWrite10 = 0x2a
	scsiSense   = 0x03
	scsiMode6   = 0x1a
	featHalt    = 0 // ENDPOINT_HALT
)

// Disk is a USB mass-storage device (logical unit 0).
type Disk struct {
	h         *hc
	d         *device
	in, out   *bulkEP
	cmd       *dma.Buffer // CBW and CSW
	data      *dma.Buffer
	tag       uint32
	statMu    sync.Mutex // stalls and lastSense: lsdev reads them
	stalls    int
	lastSense string
	retrying  bool
	wp        bool // the medium is write-protected
	bs        int
	blocks    uint64
	info      string
	readReqs  chan *readReq
	down      <-chan struct{} // closed when the controller stops
}

type bulkEP struct {
	dci  int
	ring *ring
	mps  int
}

type readReq struct {
	disk  *Disk
	lba   uint64
	p     []byte
	write bool
	done  chan error
}

func (k *Disk) BlockSize() int { return k.bs }
func (k *Disk) Blocks() uint64 { return k.blocks }
func (k *Disk) Devices() []string {
	k.statMu.Lock()
	defer k.statMu.Unlock()
	s := k.info
	if k.stalls > 0 || k.lastSense != "" {
		s += fmt.Sprintf(", %d stalls, last sense %s", k.stalls, k.lastSense)
	}
	return []string{s}
}

// ReadBlocks implements block.Device: the request goes to the controller's
// goroutine.
func (k *Disk) ReadBlocks(ctx context.Context, lba uint64, p []byte) error {
	return k.do(ctx, lba, p, false)
}

// ReadOnly reports a write-protected medium (block.Writable checks it).
func (k *Disk) ReadOnly() bool { return k.wp }

// WriteBlocks implements block.Writer (unless the medium is write-protected:
// then it fails, and Writable says so).
func (k *Disk) WriteBlocks(ctx context.Context, lba uint64, p []byte) error {
	return k.do(ctx, lba, p, true)
}

func (k *Disk) do(ctx context.Context, lba uint64, p []byte, write bool) error {
	if err := block.Check(k, lba, p); err != nil {
		return err
	}
	r := &readReq{disk: k, lba: lba, p: p, write: write, done: make(chan error, 1)}
	select {
	case k.readReqs <- r:
	case <-k.down:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-r.done
}

func (h *hc) newDisk(ctx context.Context, d *device, it usb.Interface) error {
	k := &Disk{h: h, d: d, readReqs: h.reads, down: h.down}
	for _, e := range it.Endpoints {
		var typ int
		switch {
		case e.IsBulkIn():
			typ = epBulkIn
		case e.IsBulkOut():
			typ = epBulkOut
		default:
			continue
		}
		dci, r, err := h.configureEP(ctx, d, e, typ, [3]uint32{})
		if err != nil {
			return err
		}
		ep := &bulkEP{dci: dci, ring: r, mps: e.MaxPacket}
		if typ == epBulkIn {
			k.in = ep
		} else {
			k.out = ep
		}
	}
	if k.in == nil || k.out == nil {
		return errors.New("no bulk endpoints")
	}
	var err error
	if k.cmd, err = h.alloc(64, 64); err != nil {
		return err
	}
	if k.data, err = h.alloc(maxXfer, 4096); err != nil {
		return err
	}
	inq, err := k.scsi(ctx, []byte{scsiInq, 0, 0, 0, 36, 0}, 36)
	if err != nil {
		return fmt.Errorf("inquiry: %w", err)
	}
	vendor := strings.TrimSpace(string(inq[8:16]))
	product := strings.TrimSpace(string(inq[16:32]))
	for try := 0; ; try++ { // the medium may need a moment
		if _, err = k.scsi(ctx, []byte{scsiTUR, 0, 0, 0, 0, 0}, 0); err == nil || try == 5 {
			break
		}
		k.scsi(ctx, []byte{scsiSense, 0, 0, 0, 18, 0}, 18)
		sleep(ctx, 100e6)
	}
	capb, err := k.scsi(ctx, []byte{scsiCap10, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 8)
	if err != nil {
		return fmt.Errorf("read capacity: %w", err)
	}
	k.blocks = uint64(binary.BigEndian.Uint32(capb)) + 1
	k.bs = int(binary.BigEndian.Uint32(capb[4:]))
	if k.bs < 512 || k.bs > 4096 || k.bs&(k.bs-1) != 0 {
		return fmt.Errorf("block size %d", k.bs)
	}
	// write protect: MODE SENSE(6)'s header, device-specific byte, bit 7
	if hdr, err := k.scsi(ctx, []byte{scsiMode6, 0, 0x3f, 0, 4, 0}, 4); err == nil && len(hdr) >= 3 {
		k.wp = hdr[2]&0x80 != 0
	}
	k.info = fmt.Sprintf("%s %s, %d x %d B, %s", vendor, product, k.blocks, k.bs, d.where)
	if k.wp {
		k.info += ", write-protected"
	}
	idx := len(h.disks)
	h.disks = append(h.disks, k)
	h.names.publish(h.publish, "usbdisk", idx, dev.Node{Compatible: DiskCompatible, Config: k})
	return nil
}

// bulk runs one transfer on a bulk endpoint and returns the bytes moved.
func (k *Disk) bulk(ctx context.Context, ep *bulkEP, phys uint64, n int, in bool) (int, error) {
	h := k.h
	dir := uint32(0)
	if in {
		dir = trbISP
	}
	trb := ep.ring.push(phys, uint32(n), trbNormal<<10|trbIOC|dir)
	h.r.Write32(h.db+4*uint64(k.d.slot), uint32(ep.dci))
	e, err := h.wait(ctx, "bulk transfer", func(e event) bool {
		return e.typ() == trbTransferEv && e.slot() == k.d.slot && e.ep() == ep.dci && e.param == trb
	})
	if err != nil {
		return 0, err
	}
	switch c := e.code(); c {
	case ccSuccess, ccShort:
		return n - e.residual(), nil
	case ccStall:
		k.statMu.Lock()
		k.stalls++
		k.statMu.Unlock()
		if err := k.clearStall(ctx, ep); err != nil {
			return 0, fmt.Errorf("stall recovery: %w", err)
		}
		return 0, errStall
	default:
		return 0, fmt.Errorf("bulk transfer failed with code %d", c)
	}
}

var errStall = errors.New("endpoint stalled")

// clearStall recovers a halted bulk endpoint: the xHC's endpoint is reset
// and its dequeue pointer moved to the ring's enqueue point (dropping the
// failed TRB), then the device's halt is cleared.
func (k *Disk) clearStall(ctx context.Context, ep *bulkEP) error {
	h := k.h
	if _, err := h.command(ctx, 0, 0, trbResetEP<<10|uint32(ep.dci)<<16|uint32(k.d.slot)<<24); err != nil {
		return fmt.Errorf("reset endpoint: %w", err)
	}
	deq := ep.ring.buf.Bus + uint64(ep.ring.enq*16) | uint64(ep.ring.pcs)
	if _, err := h.command(ctx, deq, 0, trbSetDequeue<<10|uint32(ep.dci)<<16|uint32(k.d.slot)<<24); err != nil {
		return fmt.Errorf("set dequeue: %w", err)
	}
	addr := uint16(ep.dci / 2)
	if ep.dci%2 == 1 {
		addr |= 0x80
	}
	_, err := h.control(ctx, k.d, usb.Setup{RequestType: 0x02, Request: reqClearFeature, Value: featHalt, Index: addr}, nil)
	return err
}

// scsi runs a command with up to n bytes of data in, returned from the
// data buffer.
func (k *Disk) scsi(ctx context.Context, cdb []byte, n int) ([]byte, error) {
	return k.scsiDir(ctx, cdb, n, true)
}

// scsiOut runs a command that sends the first n bytes of the data buffer.
func (k *Disk) scsiOut(ctx context.Context, cdb []byte, n int) error {
	_, err := k.scsiDir(ctx, cdb, n, false)
	return err
}

// scsiDir runs one command; on a failed status it asks for the sense data,
// so the error says why.
func (k *Disk) scsiDir(ctx context.Context, cdb []byte, n int, in bool) ([]byte, error) {
	b, err := k.bot(ctx, cdb, n, in)
	if err != errCheckCondition || cdb[0] == scsiSense {
		return b, err
	}
	sense, serr := k.bot(ctx, []byte{scsiSense, 0, 0, 0, 18, 0}, 18, true)
	if serr != nil || len(sense) < 14 {
		return nil, fmt.Errorf("SCSI command %#x failed", cdb[0])
	}
	key, asc := sense[2]&0xf, sense[12]
	k.statMu.Lock()
	k.lastSense = fmt.Sprintf("key %#x asc %#x/%#x", key, asc, sense[13])
	k.statMu.Unlock()
	if key == 0x6 && !k.retrying { // unit attention (medium changed, reset): once more
		k.retrying = true
		defer func() { k.retrying = false }()
		return k.scsiDir(ctx, cdb, n, in)
	}
	return nil, fmt.Errorf("SCSI command %#x failed: sense %s", cdb[0], k.lastSense)
}

var errCheckCondition = errors.New("check condition")

// bot is one Bulk-Only Transport exchange: command, data, status.
func (k *Disk) bot(ctx context.Context, cdb []byte, n int, in bool) ([]byte, error) {
	k.tag++
	cbw := k.cmd.Bytes[:31]
	clear(cbw)
	le := binary.LittleEndian
	le.PutUint32(cbw[0:], cbwSig)
	le.PutUint32(cbw[4:], k.tag)
	le.PutUint32(cbw[8:], uint32(n))
	if in {
		cbw[12] = 0x80 // data in
	}
	cbw[14] = byte(len(cdb))
	copy(cbw[15:], cdb)
	if _, err := k.bulk(ctx, k.out, k.cmd.Bus, 31, false); err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	got := 0
	if n > 0 {
		ep := k.in
		if !in {
			ep = k.out
		}
		var err error
		if got, err = k.bulk(ctx, ep, k.data.Bus, n, in); err != nil && err != errStall {
			return nil, fmt.Errorf("data: %w", err)
		}
		// a stalled data stage still ends with a status (BOT §6.7.2)
	}
	csw := k.cmd.Bytes[32:45]
	if _, err := k.bulk(ctx, k.in, k.cmd.Bus+32, 13, true); err != nil {
		if err != errStall {
			return nil, fmt.Errorf("status: %w", err)
		}
		if _, err := k.bulk(ctx, k.in, k.cmd.Bus+32, 13, true); err != nil { // BOT: retry once after clearing
			return nil, fmt.Errorf("status: %w", err)
		}
	}
	if le.Uint32(csw[0:]) != cswSig || le.Uint32(csw[4:]) != k.tag {
		return nil, errors.New("bad command status wrapper")
	}
	switch csw[12] {
	case 0:
	case 1:
		return nil, errCheckCondition
	default:
		return nil, fmt.Errorf("SCSI command %#x: phase error", cdb[0])
	}
	return k.data.Bytes[:got], nil
}

// transfer serves ReadBlocks and WriteBlocks (on the controller's
// goroutine).
func (k *Disk) transfer(ctx context.Context, lba uint64, p []byte, write bool) error {
	for len(p) > 0 {
		n := min(len(p), maxXfer)
		cnt := n / k.bs
		op := byte(scsiRead10)
		if write {
			op = scsiWrite10
		}
		cdb := []byte{op, 0, 0, 0, 0, 0, 0, byte(cnt >> 8), byte(cnt), 0}
		binary.BigEndian.PutUint32(cdb[2:], uint32(lba))
		if write {
			copy(k.data.Bytes, p[:n])
			if err := k.scsiOut(ctx, cdb, n); err != nil {
				return err
			}
		} else {
			b, err := k.scsi(ctx, cdb, n)
			if err != nil {
				return err
			}
			if len(b) != n {
				return fmt.Errorf("short read: %d of %d bytes", len(b), n)
			}
			copy(p, b)
		}
		p, lba = p[n:], lba+uint64(cnt)
	}
	return nil
}

// DiskDriver publishes a USB disk the controller found (Config: *Disk).
func DiskDriver() dev.Driver {
	return dev.Driver{Compatible: DiskCompatible, Run: func(ctx context.Context, d *dev.Device) error {
		k, ok := d.Config.(*Disk)
		if !ok {
			return errors.New("usb-storage: no disk")
		}
		d.Ready(k)
		<-ctx.Done()
		return nil
	}}
}
