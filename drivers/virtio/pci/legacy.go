// Package pci is the virtio PCI transport, legacy interface only (virtio
// 0.9.5, "transitional" devices without the 1.0 capabilities). It exists
// for Parallels Desktop on Apple silicon, whose virtio-net is such a device
// with its register block in memory BAR0 (docs/Parallels.md).
//
// Legacy register block (BAR0), little-endian:
//
//	0x00  32  device features (bits 0-31; there are no others)
//	0x04  32  driver features
//	0x08  32  queue address: page number (4096) of the selected queue
//	0x0c  16  queue size (the device's; read-only)
//	0x0e  16  queue select
//	0x10  16  queue notify (write the queue index)
//	0x12   8  device status
//	0x13   8  ISR status (reading clears it)
//	0x14      device configuration (MSI-X off; 0x18 with it on)
//
// The registers are 8 and 16 bits wide and packed: every access must be
// exactly its register's width (mmio.NarrowRegs).
package pci

import (
	"context"
	"errors"
	"fmt"
	"time"

	vmmio "godeep/drivers/virtio/mmio"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

// Legacy register offsets.
const (
	DeviceFeatures = 0x00
	DriverFeatures = 0x04
	QueuePFN       = 0x08
	QueueSize      = 0x0c
	QueueSelect    = 0x0e
	QueueNotify    = 0x10
	Status         = 0x12
	ISR            = 0x13
	Config         = 0x14
)

// VendorID is virtio's PCI vendor; legacy (transitional) device IDs are
// 0x1000 + (virtio device ID - 1): 0x1000 is net.
const VendorID = 0x1af4

// Legacy is one initialized legacy virtio-pci device. It has the methods
// virtio drivers use on a transport (as vmmio.Transport).
type Legacy struct {
	R        mmio.NarrowRegs
	Features uint64 // negotiated (bits 0-31)
}

// InitLegacy resets the device and negotiates features: whatever of want
// the device offers. There is no FEATURES_OK step in the legacy interface.
// Set up queues, then call DriverOK.
func InitLegacy(ctx context.Context, r mmio.NarrowRegs, want uint64) (*Legacy, error) {
	r.Store8(Status, 0) // reset
	for deadline := time.Now().Add(time.Second); r.Read8(Status) != 0; {
		if time.Now().After(deadline) {
			return nil, errors.New("virtio-pci: reset timed out")
		}
		if err := sleep(ctx, 100*time.Microsecond); err != nil {
			return nil, err
		}
	}
	r.Store8(Status, vmmio.StatusAcknowledge)
	r.Store8(Status, vmmio.StatusAcknowledge|vmmio.StatusDriver)
	feat := uint64(r.Read32(DeviceFeatures)) & want & 0xffffffff
	r.Write32(DriverFeatures, uint32(feat))
	return &Legacy{R: r, Features: feat}, nil
}

// DriverOK tells the device the driver is ready: queues are live.
func (t *Legacy) DriverOK() {
	t.R.Store8(Status, vmmio.StatusAcknowledge|vmmio.StatusDriver|vmmio.StatusDriverOK)
}

// Reset stops the device; it releases every queue buffer. Call it before
// freeing queue memory.
func (t *Legacy) Reset() { t.R.Store8(Status, 0) }

// Has reports whether feature bit f was negotiated.
func (t *Legacy) Has(f uint64) bool { return t.Features&f != 0 }

// Ack reads (and so clears) the interrupt causes: bit 0 used buffers,
// bit 1 configuration change, as vmmio's.
func (t *Legacy) Ack() uint32 { return uint32(t.R.Read8(ISR)) }

// Config8 reads one byte of the device configuration.
func (t *Legacy) Config8(off uint64) uint8 { return t.R.Read8(Config + off) }

// Queue sets up virtqueue index. A legacy device decides the size itself
// (q.Size; max is for the modern transport's interface): drivers size their
// buffers from q.Size.
func (t *Legacy) Queue(index, _ int, a *dma.Allocator) (*vmmio.Queue, error) {
	t.R.Write16(QueueSelect, uint16(index))
	if t.R.Read32(QueuePFN) != 0 {
		return nil, fmt.Errorf("virtio-pci: queue %d already live", index)
	}
	n := int(t.R.Read16(QueueSize))
	if n == 0 {
		return nil, fmt.Errorf("virtio-pci: queue %d not available", index)
	}
	q, err := vmmio.NewLegacyQueue(index, n, a, func() { t.R.Write16(QueueNotify, uint16(index)) })
	if err != nil {
		return nil, err
	}
	if q.Base()>>12 > 0xffffffff {
		q.Free()
		return nil, fmt.Errorf("virtio-pci: queue at %#x, beyond a legacy device's reach", q.Base())
	}
	t.R.Write16(QueueSelect, uint16(index))
	t.R.Write32(QueuePFN, uint32(q.Base()>>12))
	return q, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
