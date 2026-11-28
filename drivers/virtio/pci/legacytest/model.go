// Package legacytest models a legacy virtio-pci device (virtio 0.9.5
// register block, split queues at a page number) for host tests: registers
// with their real widths (an access of the wrong width is recorded as an
// error, as it would misbehave on the device), rings and buffers in a byte
// slice standing for physical memory (a dma.Region's Mem).
package legacytest

import (
	"encoding/binary"
	"fmt"
	"sync"

	vmmio "godeep/drivers/virtio/mmio"
	vpci "godeep/drivers/virtio/pci"
	"godeep/kernel/dma"
)

// QueueNum is every queue's size (the device decides it).
const QueueNum = 256

type queue struct {
	pfn                uint32
	lastAvail, usedIdx uint16
}

func (q *queue) desc() uint64  { return uint64(q.pfn) << 12 }
func (q *queue) avail() uint64 { return q.desc() + 16*QueueNum }
func (q *queue) used() uint64 {
	return (q.avail() + 4 + 2*QueueNum + 2 + vmmio.LegacyAlign - 1) &^ (vmmio.LegacyAlign - 1)
}

// Buf is a buffer the driver made available.
type Buf struct {
	ID    uint16
	Addr  uint64
	Len   int
	Write bool // device-writable
}

// Device is a modelled legacy virtio device. It implements mmio.NarrowRegs.
type Device struct {
	Mem  []byte
	Base uint64 // physical address of Mem[0]

	mu       sync.Mutex
	features uint32
	driver   uint32
	status   uint8
	isr      uint8
	sel      uint16
	queues   map[int]*queue
	config   []byte
	errs     []string
	// OnNotify runs (with the model locked) when the driver rings queue q.
	OnNotify func(d *Device, q int)
}

// New models a device offering features, with device configuration bytes.
func New(features uint32, config []byte, mem []byte, base uint64) *Device {
	return &Device{Mem: mem, Base: base, features: features, config: config, queues: map[int]*queue{}}
}

func (d *Device) bad(f string, a ...any) { d.errs = append(d.errs, fmt.Sprintf(f, a...)) }

// Errors lists the accesses the device would have mishandled.
func (d *Device) Errors() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.errs...)
}

// Negotiated returns the features the driver accepted.
func (d *Device) Negotiated() uint32 { d.mu.Lock(); defer d.mu.Unlock(); return d.driver }

// Status returns the device status register.
func (d *Device) Status() uint8 { d.mu.Lock(); defer d.mu.Unlock(); return d.status }

func (d *Device) q(i int) *queue {
	q := d.queues[i]
	if q == nil {
		q = &queue{}
		d.queues[i] = q
	}
	return q
}

func (d *Device) Read32(off uint64) uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch off {
	case vpci.DeviceFeatures:
		return d.features
	case vpci.DriverFeatures:
		return d.driver
	case vpci.QueuePFN:
		return d.q(int(d.sel)).pfn
	}
	d.bad("32-bit read at %#x", off)
	return 0
}

func (d *Device) Write32(off uint64, v uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch off {
	case vpci.DriverFeatures:
		if v&^d.features != 0 {
			d.bad("driver features %#x not offered (%#x)", v, d.features)
		}
		d.driver = v
	case vpci.QueuePFN:
		q := d.q(int(d.sel))
		*q = queue{pfn: v}
	default:
		d.bad("32-bit write at %#x", off)
	}
}

func (d *Device) Read16(off uint64) uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch off {
	case vpci.QueueSize:
		if d.sel < 2 {
			return QueueNum
		}
		return 0
	case vpci.QueueSelect:
		return d.sel
	}
	d.bad("16-bit read at %#x", off)
	return 0
}

func (d *Device) Write16(off uint64, v uint16) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch off {
	case vpci.QueueSelect:
		d.sel = v
	case vpci.QueueNotify:
		if d.status&vmmio.StatusDriverOK == 0 {
			d.bad("notify before DRIVER_OK")
		}
		if d.OnNotify != nil {
			d.OnNotify(d, int(v))
		}
	default:
		d.bad("16-bit write at %#x", off)
	}
}

func (d *Device) Read8(off uint64) uint8 {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case off == vpci.Status:
		return d.status
	case off == vpci.ISR:
		v := d.isr
		d.isr = 0
		return v
	case off >= vpci.Config && off < vpci.Config+uint64(len(d.config)):
		return d.config[off-vpci.Config]
	}
	d.bad("8-bit read at %#x", off)
	return 0
}

func (d *Device) Store8(off uint64, v uint8) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if off != vpci.Status {
		d.bad("8-bit write at %#x", off)
		return
	}
	if v == 0 {
		d.queues = map[int]*queue{}
		d.isr, d.driver = 0, 0
	}
	d.status = v
}

// At returns n bytes of "physical" memory at phys.
func (d *Device) At(phys uint64, n int) []byte { return d.Mem[phys-d.Base:][:n] }

// Pop takes the next available buffer of queue qi. Call with the model
// locked (from OnNotify) or through Lock/Unlock.
func (d *Device) Pop(qi int) (Buf, bool) {
	q := d.queues[qi]
	if q == nil || q.pfn == 0 {
		return Buf{}, false
	}
	le := binary.LittleEndian
	idx := uint16(dma.LoadWord(d.Mem, int(q.avail()-d.Base)) >> 16) // acquire, like the hardware
	if idx == q.lastAvail {
		return Buf{}, false
	}
	id := le.Uint16(d.At(q.avail()+4+2*uint64(int(q.lastAvail)%QueueNum), 2))
	q.lastAvail++
	desc := d.At(q.desc()+16*uint64(id), 16)
	return Buf{ID: id, Addr: le.Uint64(desc[0:]), Len: int(le.Uint32(desc[8:])),
		Write: le.Uint16(desc[12:])&vmmio.DescWrite != 0}, true
}

// Push returns buffer id to the driver with n bytes written.
func (d *Device) Push(qi int, id uint16, n int) {
	q := d.queues[qi]
	le := binary.LittleEndian
	e := d.At(q.used()+4+8*uint64(int(q.usedIdx)%QueueNum), 8)
	le.PutUint32(e[0:], uint32(id))
	le.PutUint32(e[4:], uint32(n))
	q.usedIdx++
	dma.StoreWord(d.Mem, int(q.used()-d.Base), uint32(q.usedIdx)<<16) // release
	d.isr |= vmmio.IntUsedBuffer
}

// Lock and Unlock guard the model for Pop/Push from outside OnNotify.
func (d *Device) Lock()   { d.mu.Lock() }
func (d *Device) Unlock() { d.mu.Unlock() }
