package xhci

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"godeep/drivers/usb"
)

// USB Attached SCSI (UAS), what USB-C SSDs and fast enclosures speak: SCSI
// commands as information units (IUs) over four bulk pipes, command,
// status, data-in and data-out. Without bulk streams (USB 2, and how this
// driver runs) one command is in flight: the host sends a Command IU; for
// a data phase the device first answers Read Ready / Write Ready on the
// status pipe; the data moves; a Sense IU ends the command, carrying the
// sense data on failure (no REQUEST SENSE round trip). The SCSI layer
// above (probe, READ/WRITE(10), unit attention) is the one BOT disks use.
//
// SuperSpeed UAS needs streams; on a SuperSpeed link this driver uses the
// device's Bulk-Only alternate setting instead (every UAS device has one).

const (
	iuCommand    = 0x01
	iuSense      = 0x03
	iuResponse   = 0x04
	iuReadReady  = 0x06
	iuWriteReady = 0x07
	statSize     = 96 // a Sense IU: 16-byte header + sense data
)

// uasAlt finds a UAS alternate setting of interface number num.
func uasAlt(c usb.Config, num byte) (usb.Interface, bool) {
	for _, it := range c.Interfaces {
		if it.Number == num && it.Class == usb.ClassMassStorage && it.Subclass == 6 && it.Protocol == usb.ProtoUAS {
			return it, true
		}
	}
	return usb.Interface{}, false
}

// newUAS drives a UAS interface (selecting its alternate setting first).
func (h *hc) newUAS(ctx context.Context, d *device, it usb.Interface) error {
	if it.Alt != 0 {
		if _, err := h.control(ctx, d, usb.Setup{RequestType: 0x01, Request: usb.ReqSetInterface,
			Value: uint16(it.Alt), Index: uint16(it.Number)}, nil); err != nil {
			return fmt.Errorf("set interface: %w", err)
		}
	}
	k := &Disk{h: h, d: d, readReqs: h.reads, down: h.down, gone: make(chan struct{})}
	for _, e := range it.Endpoints {
		typ := epBulkOut
		if e.IsBulkIn() {
			typ = epBulkIn
		} else if !e.IsBulkOut() {
			continue
		}
		if e.PipeID < usb.PipeCommand || e.PipeID > usb.PipeDataOut {
			continue
		}
		dci, r, err := h.configureEP(ctx, d, e, typ, [3]uint32{})
		if err != nil {
			return err
		}
		k.uasEP[e.PipeID] = &bulkEP{dci: dci, ring: r, mps: e.MaxPacket}
	}
	for id := usb.PipeCommand; id <= usb.PipeDataOut; id++ {
		if k.uasEP[id] == nil {
			return fmt.Errorf("UAS: no pipe %d", id)
		}
	}
	var err error
	if k.stat, err = h.alloc(statSize, 64); err != nil {
		return err
	}
	k.xfer, k.proto = k.uas, "UAS"
	return h.probeDisk(ctx, d, k)
}

// uasTag numbers commands 1..0xfffe (0 and 0xffff are reserved).
func (k *Disk) uasTag() uint16 {
	k.tag++
	return uint16(k.tag%0xfffe) + 1
}

// readStatus reads the next IU from the status pipe for tag.
func (k *Disk) readStatus(ctx context.Context, tag uint16) (byte, []byte, error) {
	n, err := k.bulk(ctx, k.uasEP[usb.PipeStatus], k.stat.Bus, statSize, true)
	if err != nil {
		return 0, nil, fmt.Errorf("status: %w", err)
	}
	b := k.stat.Bytes[:n]
	if n < 4 {
		return 0, nil, fmt.Errorf("status: short IU (%d bytes)", n)
	}
	if got := binary.BigEndian.Uint16(b[2:]); got != tag {
		return 0, nil, fmt.Errorf("status: IU for tag %d, want %d", got, tag)
	}
	return b[0], b, nil
}

// uas runs one command: Command IU, (Read/Write Ready, data), Sense IU.
func (k *Disk) uas(ctx context.Context, cdb []byte, n int, in bool) ([]byte, error) {
	if len(cdb) > 16 {
		return nil, errors.New("UAS: CDB longer than 16 bytes")
	}
	tag := k.uasTag()
	iu := k.cmd.Bytes[:32]
	clear(iu)
	iu[0] = iuCommand
	binary.BigEndian.PutUint16(iu[2:], tag)
	// iu[4]: task attribute 0 (simple); iu[8:16]: LUN 0
	copy(iu[16:], cdb)
	if _, err := k.bulk(ctx, k.uasEP[usb.PipeCommand], k.cmd.Bus, 32, false); err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	got := 0
	id, b, err := k.readStatus(ctx, tag)
	if err != nil {
		return nil, err
	}
	if n > 0 && id != iuSense { // the device may fail the command before any data
		want, pipe := byte(iuReadReady), k.uasEP[usb.PipeDataIn]
		if !in {
			want, pipe = iuWriteReady, k.uasEP[usb.PipeDataOut]
		}
		if id != want {
			return nil, fmt.Errorf("UAS: IU %#x, want %#x", id, want)
		}
		if got, err = k.bulk(ctx, pipe, k.data.Bus, n, in); err != nil && err != errStall {
			return nil, fmt.Errorf("data: %w", err)
		}
		if id, b, err = k.readStatus(ctx, tag); err != nil {
			return nil, err
		}
	}
	switch id {
	case iuSense:
	case iuResponse:
		return nil, fmt.Errorf("UAS: response IU, code %#x", b[len(b)-1])
	default:
		return nil, fmt.Errorf("UAS: IU %#x, want a Sense IU", id)
	}
	if len(b) < 16 {
		return nil, errors.New("UAS: short Sense IU")
	}
	switch b[6] { // SCSI status
	case 0:
		return k.data.Bytes[:got], nil
	case 2: // CHECK CONDITION: the sense data is right here
		l := min(int(binary.BigEndian.Uint16(b[14:])), len(b)-16)
		k.autoSense = append([]byte(nil), b[16:16+l]...)
		return nil, errCheckCondition
	default:
		return nil, fmt.Errorf("SCSI command %#x: status %#x", cdb[0], b[6])
	}
}
