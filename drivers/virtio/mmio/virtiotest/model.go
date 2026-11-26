// Package virtiotest models a virtio-mmio device (version 2, split queues)
// for host tests: registers on an mmio.Fake, rings and buffers in a byte
// slice standing for physical memory (a dma.Region's Mem).
package virtiotest

import (
	"encoding/binary"
	"sync"

	vmmio "godeep/drivers/virtio/mmio"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

// Queue is the device's view of one virtqueue.
type Queue struct {
	Num                int
	Desc, Avail, Used  uint64
	Ready              bool
	lastAvail, usedIdx uint16
}

// Buf is a buffer the driver made available.
type Buf struct {
	ID    uint16
	Addr  uint64
	Len   int
	Write bool // device-writable
}

// Device is a modelled virtio device.
type Device struct {
	F    *mmio.Fake
	Mem  []byte
	Base uint64 // physical address of Mem[0]

	mu       sync.Mutex
	features uint64
	driver   uint64
	queues   map[int]*Queue
	config   []byte
	// OnNotify runs (with the model locked) when the driver rings queue q.
	OnNotify func(d *Device, q int)
	// OnInterrupt is called (unlocked) when the device raises an interrupt.
	OnInterrupt func()
	Notifies    int
}

// New models device id with the given feature bits (VERSION_1 is added) and
// device configuration bytes.
func New(id uint32, features uint64, config []byte, mem []byte, base uint64) *Device {
	d := &Device{F: mmio.NewFake(), Mem: mem, Base: base, features: features | vmmio.FeatureVersion1,
		queues: map[int]*Queue{}, config: config}
	f := d.F
	f.Set(vmmio.MagicValue, vmmio.Magic)
	f.Set(vmmio.Version, 2)
	f.Set(vmmio.DeviceID, id)
	f.Set(vmmio.VendorID, 0x554d4551)
	f.OnRead = func(f *mmio.Fake, off uint64) (uint32, bool) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case off == vmmio.DeviceFeatures:
			return uint32(d.features >> (32 * f.Get(vmmio.DeviceFeaturesSel))), true
		case off == vmmio.QueueNumMax:
			return 256, true
		case off == vmmio.QueueReady:
			q := d.queues[int(f.Get(vmmio.QueueSel))]
			if q != nil && q.Ready {
				return 1, true
			}
			return 0, true
		case off >= vmmio.Config && off < vmmio.Config+uint64(len(d.config)):
			var w [4]byte
			copy(w[:], d.config[off-vmmio.Config:])
			return binary.LittleEndian.Uint32(w[:]), true
		}
		return 0, false
	}
	f.OnWrite = func(f *mmio.Fake, off uint64, v uint32) bool {
		d.mu.Lock()
		q := d.q(int(f.Get(vmmio.QueueSel)))
		switch off {
		case vmmio.DriverFeatures:
			if f.Get(vmmio.DriverFeaturesSel) == 1 {
				d.driver = d.driver&0xffffffff | uint64(v)<<32
			} else {
				d.driver = d.driver&^0xffffffff | uint64(v)
			}
		case vmmio.Status:
			if v == 0 {
				d.queues = map[int]*Queue{}
				f.Set(vmmio.InterruptStatus, 0)
			}
			if v&vmmio.StatusFeaturesOK != 0 && d.driver&^d.features != 0 {
				v &^= vmmio.StatusFeaturesOK // unknown features: refuse
			}
			f.Set(vmmio.Status, v)
		case vmmio.QueueNum:
			q.Num = int(v)
		case vmmio.QueueDescLow:
			q.Desc = q.Desc&^0xffffffff | uint64(v)
		case vmmio.QueueDescHigh:
			q.Desc = q.Desc&0xffffffff | uint64(v)<<32
		case vmmio.QueueDriverLow:
			q.Avail = q.Avail&^0xffffffff | uint64(v)
		case vmmio.QueueDriverHigh:
			q.Avail = q.Avail&0xffffffff | uint64(v)<<32
		case vmmio.QueueDeviceLow:
			q.Used = q.Used&^0xffffffff | uint64(v)
		case vmmio.QueueDeviceHigh:
			q.Used = q.Used&0xffffffff | uint64(v)<<32
		case vmmio.QueueReady:
			q.Ready = v == 1
		case vmmio.InterruptACK:
			f.Set(vmmio.InterruptStatus, f.Get(vmmio.InterruptStatus)&^v)
		case vmmio.QueueNotify:
			d.Notifies++
			if d.OnNotify != nil {
				d.OnNotify(d, int(v))
			}
		default:
			d.mu.Unlock()
			return false
		}
		d.mu.Unlock()
		return true
	}
	return d
}

func (d *Device) q(i int) *Queue {
	q := d.queues[i]
	if q == nil {
		q = &Queue{}
		d.queues[i] = q
	}
	return q
}

// Negotiated returns the features the driver accepted.
func (d *Device) Negotiated() uint64 { d.mu.Lock(); defer d.mu.Unlock(); return d.driver }

// At returns n bytes of "physical" memory at phys.
func (d *Device) At(phys uint64, n int) []byte { return d.Mem[phys-d.Base:][:n] }

// PopChain takes the next available request as its chain of buffers.
func (d *Device) PopChain(qi int) ([]Buf, bool) {
	b, ok := d.Pop(qi)
	if !ok {
		return nil, false
	}
	q := d.queues[qi]
	le := binary.LittleEndian
	out := []Buf{b}
	for id := b.ID; ; {
		desc := d.At(q.Desc+16*uint64(id), 16)
		if le.Uint16(desc[12:])&vmmio.DescNext == 0 {
			return out, true
		}
		id = le.Uint16(desc[14:])
		nd := d.At(q.Desc+16*uint64(id), 16)
		out = append(out, Buf{ID: id, Addr: le.Uint64(nd[0:]), Len: int(le.Uint32(nd[8:])),
			Write: le.Uint16(nd[12:])&vmmio.DescWrite != 0})
	}
}

// Pop takes the next available buffer of queue qi. Call with the model
// locked (from OnNotify) or through Lock/Unlock.
func (d *Device) Pop(qi int) (Buf, bool) {
	q := d.queues[qi]
	if q == nil || !q.Ready {
		return Buf{}, false
	}
	le := binary.LittleEndian
	idx := uint16(dma.LoadWord(d.Mem, int(q.Avail-d.Base)) >> 16) // acquire, like the hardware
	if idx == q.lastAvail {
		return Buf{}, false
	}
	id := le.Uint16(d.At(q.Avail+4+2*uint64(int(q.lastAvail)%q.Num), 2))
	q.lastAvail++
	desc := d.At(q.Desc+16*uint64(id), 16)
	return Buf{ID: id, Addr: le.Uint64(desc[0:]), Len: int(le.Uint32(desc[8:])),
		Write: le.Uint16(desc[12:])&vmmio.DescWrite != 0}, true
}

// Push returns buffer id to the driver with n bytes written.
func (d *Device) Push(qi int, id uint16, n int) {
	q := d.queues[qi]
	le := binary.LittleEndian
	e := d.At(q.Used+4+8*uint64(int(q.usedIdx)%q.Num), 8)
	le.PutUint32(e[0:], uint32(id))
	le.PutUint32(e[4:], uint32(n))
	q.usedIdx++
	dma.StoreWord(d.Mem, int(q.Used-d.Base), uint32(q.usedIdx)<<16) // release
	d.F.Set(vmmio.InterruptStatus, d.F.Get(vmmio.InterruptStatus)|vmmio.IntUsedBuffer)
}

// Lock and Unlock guard the model for Pop/Push from outside OnNotify.
func (d *Device) Lock()   { d.mu.Lock() }
func (d *Device) Unlock() { d.mu.Unlock() }

// Interrupt raises the interrupt callback (call unlocked).
func (d *Device) Interrupt() {
	if d.OnInterrupt != nil {
		d.OnInterrupt()
	}
}
