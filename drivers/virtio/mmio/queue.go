package mmio

import (
	"encoding/binary"
	"errors"
	"fmt"

	"godeep/kernel/dma"
)

// Descriptor flags.
const (
	DescNext  = 1
	DescWrite = 2 // the device writes this buffer (receive side)
)

// Queue is a split virtqueue, owned by one driver goroutine (not safe for
// concurrent use).
//
// Memory: the descriptor table, the available ring (driver → device) and
// the used ring (device → driver) are each a DMA buffer. Each descriptor
// here carries one buffer (no chains), which is enough for net and rng: a
// request is "this buffer", the device answers with its index and a length.
//
// Ordering: ring entries are written with plain stores, then published with
// one release store of the index word (dma.Buffer.Store32), and the device's
// used index is read with an acquire load (Load32). The doorbell write that
// follows is an MMIO store, ordered after the release.
type Queue struct {
	Index int
	Size  int

	desc, avail, used *dma.Buffer
	whole             *dma.Buffer // legacy layout: one block the three are views of
	notify            func()

	free     []uint16 // free descriptor indices
	availIdx uint16   // next available-ring slot (free-running)
	flags    uint32   // available-ring flags (NO_INTERRUPT)
	usedIdx  uint16   // next used-ring entry to read (free-running)
}

const (
	descSize = 16
	// The available ring's index is the high half of its first word, the
	// used ring's too: flags (16) | idx (16) << 16.
	usedElemSize = 8
)

func newQueue(index, n int, a *dma.Allocator) (*Queue, error) {
	if a == nil {
		return nil, errors.New("virtio: the board has no DMA region")
	}
	q := &Queue{Index: index, Size: n}
	var err error
	if q.desc, err = a.Alloc(descSize*n, 16); err != nil {
		return nil, err
	}
	if q.avail, err = a.Alloc(4+2*n+2, 4); err != nil {
		q.Free()
		return nil, err
	}
	if q.used, err = a.Alloc(4+usedElemSize*n+2, 4); err != nil {
		q.Free()
		return nil, err
	}
	clear(q.desc.Bytes)
	clear(q.avail.Bytes)
	clear(q.used.Bytes)
	for i := n - 1; i >= 0; i-- {
		q.free = append(q.free, uint16(i))
	}
	return q, nil
}

// LegacyAlign is where a legacy (virtio 0.9.5) device expects the used
// ring: the first 4096-byte boundary after the available ring.
const LegacyAlign = 4096

// NewLegacyQueue lays out a virtqueue for a legacy transport (virtio-pci
// before 1.0): descriptors, available ring and, on the next LegacyAlign
// boundary, the used ring, in one page-aligned block whose address alone
// (Base) tells the device where all three are. n is the device's queue
// size (a legacy driver can't choose it); notify rings its doorbell.
func NewLegacyQueue(index, n int, a *dma.Allocator, notify func()) (*Queue, error) {
	if a == nil {
		return nil, errors.New("virtio: the board has no DMA region")
	}
	if n <= 0 || n&(n-1) != 0 || n > 32768 {
		return nil, fmt.Errorf("virtio: legacy queue size %d", n)
	}
	align := func(x int) int { return (x + LegacyAlign - 1) &^ (LegacyAlign - 1) }
	usedOff := align(descSize*n + 4 + 2*n + 2)
	size := usedOff + align(4+usedElemSize*n+2)
	b, err := a.Alloc(size, LegacyAlign)
	if err != nil {
		return nil, err
	}
	clear(b.Bytes)
	q := &Queue{Index: index, Size: n, whole: b, notify: notify,
		desc:  b.View(0, descSize*n),
		avail: b.View(descSize*n, 4+2*n+2),
		used:  b.View(usedOff, 4+usedElemSize*n+2),
	}
	for i := n - 1; i >= 0; i-- {
		q.free = append(q.free, uint16(i))
	}
	return q, nil
}

// Base is the device address of a legacy queue's block (NewLegacyQueue).
func (q *Queue) Base() uint64 { return q.desc.Bus }

// Free returns the queue's memory. Reset the device first.
func (q *Queue) Free() {
	if q.whole != nil {
		q.whole.Free()
		q.whole, q.desc, q.avail, q.used = nil, nil, nil, nil
		return
	}
	for _, b := range []*dma.Buffer{q.desc, q.avail, q.used} {
		if b != nil {
			b.Free()
		}
	}
	q.desc, q.avail, q.used = nil, nil, nil
}

// Free descriptors left.
func (q *Queue) Avail() int { return len(q.free) }

// Take reserves a free descriptor; fill it with Push. ok is false when the
// queue is full. Drivers that keep one buffer slot per descriptor use the
// index to find the slot.
func (q *Queue) Take() (id uint16, ok bool) {
	if len(q.free) == 0 {
		return 0, false
	}
	id = q.free[len(q.free)-1]
	q.free = q.free[:len(q.free)-1]
	return id, true
}

// Push makes a taken descriptor available: phys/n is the device-visible
// buffer, write marks one the device fills. Call Kick to tell the device.
func (q *Queue) Push(id uint16, phys uint64, n int, write bool) {
	d := q.desc.Bytes[int(id)*descSize:][:descSize]
	le := binary.LittleEndian
	le.PutUint64(d[0:], phys)
	le.PutUint32(d[8:], uint32(n))
	flags := uint16(0)
	if write {
		flags = DescWrite
	}
	le.PutUint16(d[12:], flags)
	le.PutUint16(d[14:], 0)
	slot := int(q.availIdx) % q.Size
	le.PutUint16(q.avail.Bytes[4+2*slot:], id)
	q.availIdx++
}

// Add is Take + Push: it returns the descriptor index, which comes back
// from Used.
func (q *Queue) Add(phys uint64, n int, write bool) (uint16, error) {
	id, ok := q.Take()
	if !ok {
		return 0, errors.New("virtio: queue full")
	}
	q.Push(id, phys, n, write)
	return id, nil
}

// Kick publishes added buffers and notifies the device.
func (q *Queue) Kick() {
	q.avail.Store32(0, uint32(q.availIdx)<<16|q.flags) // release: entries first, then idx
	q.notify()
}

// Used returns the next buffer the device has finished with: its descriptor
// index and the number of bytes the device wrote. ok is false when there is
// none. The descriptor is free again afterwards.
func (q *Queue) Used() (id uint16, n int, ok bool) {
	devIdx := uint16(q.used.Load32(0) >> 16) // acquire
	if devIdx == q.usedIdx {
		return 0, 0, false
	}
	e := q.used.Bytes[4+usedElemSize*(int(q.usedIdx)%q.Size):][:usedElemSize]
	q.usedIdx++
	id32 := binary.LittleEndian.Uint32(e[0:])
	n = int(binary.LittleEndian.Uint32(e[4:]))
	if id32 >= uint32(q.Size) {
		panic(fmt.Sprintf("virtio: device returned descriptor %d of %d", id32, q.Size))
	}
	// Free the whole chain: follow the NEXT links the driver wrote (the
	// device doesn't change the descriptor table).
	for d := uint16(id32); ; {
		q.free = append(q.free, d)
		desc := q.desc.Bytes[int(d)*descSize:][:descSize]
		if binary.LittleEndian.Uint16(desc[12:])&DescNext == 0 {
			break
		}
		d = binary.LittleEndian.Uint16(desc[14:])
	}
	return uint16(id32), n, true
}

// Seg is one buffer of a descriptor chain.
type Seg struct {
	Phys  uint64
	Len   int
	Write bool // the device writes it (responses come after requests)
}

// AddChain queues one request made of several buffers (e.g. a 9P request
// the device reads, then room for its reply). It returns the head
// descriptor, which comes back from Used; the whole chain is freed then.
func (q *Queue) AddChain(segs []Seg) (uint16, error) {
	if len(segs) == 0 || len(q.free) < len(segs) {
		return 0, errors.New("virtio: queue full")
	}
	ids := make([]uint16, len(segs))
	for i := range ids {
		ids[i], _ = q.Take()
	}
	le := binary.LittleEndian
	for i, sg := range segs {
		d := q.desc.Bytes[int(ids[i])*descSize:][:descSize]
		le.PutUint64(d[0:], sg.Phys)
		le.PutUint32(d[8:], uint32(sg.Len))
		flags, next := uint16(0), uint16(0)
		if sg.Write {
			flags |= DescWrite
		}
		if i+1 < len(segs) {
			flags |= DescNext
			next = ids[i+1]
		}
		le.PutUint16(d[12:], flags)
		le.PutUint16(d[14:], next)
	}
	slot := int(q.availIdx) % q.Size
	le.PutUint16(q.avail.Bytes[4+2*slot:], ids[0])
	q.availIdx++
	return ids[0], nil
}

// SuppressInterrupts asks the device not to interrupt for this queue (the
// driver polls). It's a hint: the device may still interrupt.
func (q *Queue) SuppressInterrupts(on bool) {
	q.flags = 0
	if on {
		q.flags = 1 // VIRTQ_AVAIL_F_NO_INTERRUPT
	}
	q.avail.Store32(0, uint32(q.availIdx)<<16|q.flags)
}
