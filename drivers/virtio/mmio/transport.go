package mmio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

// Transport registers (virtio 1.x, "modern" virtio-mmio, Version 2).
const (
	DeviceFeatures    = 0x010
	DeviceFeaturesSel = 0x014
	DriverFeatures    = 0x020
	DriverFeaturesSel = 0x024
	QueueSel          = 0x030
	QueueNumMax       = 0x034
	QueueNum          = 0x038
	QueueReady        = 0x044
	QueueNotify       = 0x050
	InterruptStatus   = 0x060
	InterruptACK      = 0x064
	Status            = 0x070
	QueueDescLow      = 0x080
	QueueDescHigh     = 0x084
	QueueDriverLow    = 0x090 // the available ring
	QueueDriverHigh   = 0x094
	QueueDeviceLow    = 0x0a0 // the used ring
	QueueDeviceHigh   = 0x0a4
	ConfigGeneration  = 0x0fc
	Config            = 0x100 // device-specific configuration
)

// Status bits.
const (
	StatusAcknowledge = 1
	StatusDriver      = 2
	StatusDriverOK    = 4
	StatusFeaturesOK  = 8
	StatusNeedsReset  = 64
	StatusFailed      = 128
)

// FeatureVersion1 (bit 32) marks a modern (virtio 1.x) device. The driver
// must accept it.
const FeatureVersion1 = 1 << 32

// Interrupt causes (InterruptStatus).
const (
	IntUsedBuffer   = 1
	IntConfigChange = 2
)

// Compatible is the device-manager compatible string for a virtio device
// behind a virtio-mmio slot, after the Linux binding: "virtio,device1" for
// net, "virtio,device4" for rng. Boards rewrite a "virtio,mmio" node to this
// when a driver for the device ID is registered.
func Compatible(deviceID uint32) string { return fmt.Sprintf("virtio,device%x", deviceID) }

// ID reads the device ID behind a slot (0 if empty or not virtio).
func ID(r mmio.Regs) uint32 {
	if r.Read32(MagicValue) != Magic {
		return 0
	}
	return r.Read32(DeviceID)
}

// ErrLegacy is returned for a Version 1 ("legacy") transport. QEMU's
// virtio-mmio is legacy by default; start it with
// -global virtio-mmio.force-legacy=false.
var ErrLegacy = errors.New("virtio-mmio: legacy (version 1) transport; start QEMU with -global virtio-mmio.force-legacy=false")

// Transport is one initialized virtio-mmio device.
type Transport struct {
	R        mmio.Regs
	Features uint64 // negotiated
}

// Init resets the device and negotiates features: the driver accepts
// FeatureVersion1 plus whatever of want the device offers, and returns the
// result. The device is left in FEATURES_OK; set up queues, then call
// DriverOK.
func Init(ctx context.Context, r mmio.Regs, want uint64) (*Transport, error) {
	if r.Read32(MagicValue) != Magic {
		return nil, errors.New("virtio-mmio: bad magic")
	}
	if v := r.Read32(Version); v != 2 {
		if v == 1 {
			return nil, ErrLegacy
		}
		return nil, fmt.Errorf("virtio-mmio: unknown transport version %d", v)
	}
	r.Write32(Status, 0) // reset
	deadline := time.Now().Add(time.Second)
	for r.Read32(Status) != 0 {
		if time.Now().After(deadline) {
			return nil, errors.New("virtio-mmio: reset timed out")
		}
		if err := sleep(ctx, 100*time.Microsecond); err != nil {
			return nil, err
		}
	}
	r.Write32(Status, StatusAcknowledge)
	r.Write32(Status, StatusAcknowledge|StatusDriver)

	r.Write32(DeviceFeaturesSel, 0)
	dev := uint64(r.Read32(DeviceFeatures))
	r.Write32(DeviceFeaturesSel, 1)
	dev |= uint64(r.Read32(DeviceFeatures)) << 32
	if dev&FeatureVersion1 == 0 {
		r.Write32(Status, StatusFailed)
		return nil, errors.New("virtio-mmio: device doesn't offer VERSION_1")
	}
	feat := dev & (want | FeatureVersion1)
	r.Write32(DriverFeaturesSel, 0)
	r.Write32(DriverFeatures, uint32(feat))
	r.Write32(DriverFeaturesSel, 1)
	r.Write32(DriverFeatures, uint32(feat>>32))
	r.Write32(Status, StatusAcknowledge|StatusDriver|StatusFeaturesOK)
	if r.Read32(Status)&StatusFeaturesOK == 0 {
		r.Write32(Status, StatusFailed)
		return nil, fmt.Errorf("virtio-mmio: device rejected features %#x", feat)
	}
	return &Transport{R: r, Features: feat}, nil
}

// DriverOK tells the device the driver is ready: queues are live.
func (t *Transport) DriverOK() {
	t.R.Write32(Status, StatusAcknowledge|StatusDriver|StatusFeaturesOK|StatusDriverOK)
}

// Reset stops the device; it releases every queue buffer. Call it before
// freeing queue memory.
func (t *Transport) Reset() { t.R.Write32(Status, 0) }

// Has reports whether feature bit f was negotiated.
func (t *Transport) Has(f uint64) bool { return t.Features&f != 0 }

// Ack reads and acknowledges pending interrupt causes.
func (t *Transport) Ack() uint32 {
	s := t.R.Read32(InterruptStatus)
	if s != 0 {
		t.R.Write32(InterruptACK, s)
	}
	return s
}

// Config32 reads a 32-bit word of the device configuration at off, retrying
// if the device changed it during the read (ConfigGeneration).
func (t *Transport) Config32(off uint64) uint32 {
	for {
		g := t.R.Read32(ConfigGeneration)
		v := t.R.Read32(Config + off)
		if t.R.Read32(ConfigGeneration) == g {
			return v
		}
	}
}

// Config8 reads one byte of the device configuration: byte-wide where the
// window allows it (QEMU answers all-ones to a 32-bit read that runs past
// the end of the configuration), else from the 32-bit word.
func (t *Transport) Config8(off uint64) uint8 {
	if b, ok := t.R.(mmio.ByteRegs); ok {
		return b.Read8(Config + off)
	}
	return uint8(t.Config32(off&^3) >> (8 * (off & 3)))
}

// Queue sets up virtqueue index with up to max entries (fewer if the device
// offers fewer; rounded down to a power of two).
func (t *Transport) Queue(index, max int, a *dma.Allocator) (*Queue, error) {
	t.R.Write32(QueueSel, uint32(index))
	if t.R.Read32(QueueReady) != 0 {
		return nil, fmt.Errorf("virtio-mmio: queue %d already live", index)
	}
	n := int(t.R.Read32(QueueNumMax))
	if n == 0 {
		return nil, fmt.Errorf("virtio-mmio: queue %d not available", index)
	}
	n = min(n, max)
	for n&(n-1) != 0 {
		n &= n - 1
	}
	q, err := newQueue(index, n, a)
	if err != nil {
		return nil, err
	}
	q.notify = func() { t.R.Write32(QueueNotify, uint32(index)) }
	t.R.Write32(QueueNum, uint32(n))
	t.R.Write32(QueueDescLow, uint32(q.desc.Phys))
	t.R.Write32(QueueDescHigh, uint32(q.desc.Phys>>32))
	t.R.Write32(QueueDriverLow, uint32(q.avail.Phys))
	t.R.Write32(QueueDriverHigh, uint32(q.avail.Phys>>32))
	t.R.Write32(QueueDeviceLow, uint32(q.used.Phys))
	t.R.Write32(QueueDeviceHigh, uint32(q.used.Phys>>32))
	t.R.Write32(QueueReady, 1)
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
