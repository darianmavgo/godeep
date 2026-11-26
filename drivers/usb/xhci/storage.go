package xhci

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"godeep/drivers/usb"
	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
)

// USB mass storage, Bulk-Only Transport (BOT) with SCSI commands,
// read-only: INQUIRY, TEST UNIT READY, READ CAPACITY(10), READ(10). Each
// disk is published as its own device ("usbdisk0", compatible
// DiskCompatible) whose service is a block.Device, so lsblk, partitions and
// the FAT/ext4 readers see it like an NVMe disk. Reads are carried out by
// the controller's goroutine (it owns every ring).

// DiskCompatible is the device-manager entry for a USB disk; boards
// register DiskDriver for it.
const DiskCompatible = "godeep,usb-storage"

const (
	cbwSig     = 0x43425355 // "USBC"
	cswSig     = 0x53425355 // "USBS"
	maxXfer    = 64 << 10   // bytes per READ(10)
	scsiTUR    = 0x00
	scsiInq    = 0x12
	scsiCap10  = 0x25
	scsiRead10 = 0x28
	scsiSense  = 0x03
)

// Disk is a USB mass-storage device (logical unit 0).
type Disk struct {
	h        *hc
	d        *device
	in, out  *bulkEP
	cmd      *dma.Buffer // CBW and CSW
	data     *dma.Buffer
	tag      uint32
	bs       int
	blocks   uint64
	info     string
	readReqs chan *readReq
}

type bulkEP struct {
	dci  int
	ring *ring
	mps  int
}

type readReq struct {
	disk *Disk
	lba  uint64
	p    []byte
	done chan error
}

func (k *Disk) BlockSize() int    { return k.bs }
func (k *Disk) Blocks() uint64    { return k.blocks }
func (k *Disk) Devices() []string { return []string{k.info} }

// ReadBlocks implements block.Device: the request goes to the controller's
// goroutine.
func (k *Disk) ReadBlocks(ctx context.Context, lba uint64, p []byte) error {
	if err := block.Check(k, lba, p); err != nil {
		return err
	}
	r := &readReq{disk: k, lba: lba, p: p, done: make(chan error, 1)}
	select {
	case k.readReqs <- r:
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-r.done
}

func (h *hc) newDisk(ctx context.Context, d *device, it usb.Interface) error {
	k := &Disk{h: h, d: d, readReqs: h.reads}
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
		dci, r, err := h.configureEP(ctx, d, e, typ, [2]uint32{})
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
	name := fmt.Sprintf("usbdisk%d", len(h.disks))
	k.info = fmt.Sprintf("%s %s, %d x %d B, %s", vendor, product, k.blocks, k.bs, d.where)
	h.disks = append(h.disks, k)
	if h.addDev != nil {
		h.addDev(dev.Node{Name: name, Compatible: DiskCompatible, Config: k})
	}
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
	if c := e.code(); c != ccSuccess && c != ccShort {
		return 0, fmt.Errorf("bulk transfer failed with code %d", c)
	}
	return n - e.residual(), nil
}

// scsi runs a command with up to n bytes of data in, returned from the
// data buffer.
func (k *Disk) scsi(ctx context.Context, cdb []byte, n int) ([]byte, error) {
	k.tag++
	cbw := k.cmd.Bytes[:31]
	clear(cbw)
	le := binary.LittleEndian
	le.PutUint32(cbw[0:], cbwSig)
	le.PutUint32(cbw[4:], k.tag)
	le.PutUint32(cbw[8:], uint32(n))
	cbw[12] = 0x80 // data in
	cbw[14] = byte(len(cdb))
	copy(cbw[15:], cdb)
	if _, err := k.bulk(ctx, k.out, k.cmd.Phys, 31, false); err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	got := 0
	if n > 0 {
		var err error
		if got, err = k.bulk(ctx, k.in, k.data.Phys, n, true); err != nil {
			return nil, fmt.Errorf("data: %w", err)
		}
	}
	csw := k.cmd.Bytes[32:45]
	if _, err := k.bulk(ctx, k.in, k.cmd.Phys+32, 13, true); err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	if le.Uint32(csw[0:]) != cswSig || le.Uint32(csw[4:]) != k.tag {
		return nil, errors.New("bad command status wrapper")
	}
	if st := csw[12]; st != 0 {
		return nil, fmt.Errorf("SCSI command %#x failed (status %d)", cdb[0], st)
	}
	return k.data.Bytes[:got], nil
}

// read serves ReadBlocks (on the controller's goroutine).
func (k *Disk) read(ctx context.Context, lba uint64, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), maxXfer)
		cnt := n / k.bs
		cdb := []byte{scsiRead10, 0, 0, 0, 0, 0, 0, byte(cnt >> 8), byte(cnt), 0}
		binary.BigEndian.PutUint32(cdb[2:], uint32(lba))
		b, err := k.scsi(ctx, cdb, n)
		if err != nil {
			return err
		}
		if len(b) != n {
			return fmt.Errorf("short read: %d of %d bytes", len(b), n)
		}
		copy(p, b)
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
