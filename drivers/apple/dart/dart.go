// Package dart programs the Apple t6000 DART (the IOMMU in front of the
// M1 Pro's USB controllers), after m1n1's dart.c. godeep maps the board's
// whole DMA region linearly at one bus address, once, so drivers never map
// per buffer: a dma.Allocator view (Translated) gives each buffer its
// device address. Tables use 16 KB pages: an L1 table of 2048 entries per
// TTBR, each pointing at an L2 table of 2048 page entries (32 MB). One TTBR
// covers 64 GB of bus addresses; godeep uses TTBR 0 only.
package dart

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

// Registers (t8020 layout, which the t6000 DART shares).
const (
	STREAM_COMMAND  = 0x20
	STREAM_SELECT   = 0x34
	ERROR           = 0x40 // write 1 to clear
	ERROR_ADDR_LO   = 0x50
	ERROR_ADDR_HI   = 0x54
	CONFIG          = 0x60
	ENABLED_STREAMS = 0xfc
	TCR0            = 0x100 // + 4 per stream
	TTBR0           = 0x200 // + 16 per stream, + 4 per TTBR

	CMD_INVALIDATE   = 1 << 20
	CMD_BUSY         = 1 << 2
	CONFIG_LOCK      = 1 << 15
	TCR_TRANSLATE    = 1 << 7
	TTBR_VALID       = 1 << 31
	ERROR_FLAG       = 1 << 31
	PTE_VALID        = 1 << 0
	pteSubpageEnd    = 0xfff << 40 // the whole page is accessible
	PageSize         = 16384
	entries          = 2048
	ttbrSpan         = PageSize * entries * entries // 64 GB
	maxStreams       = 16
	invalidateTimout = 10 * time.Millisecond
)

func pte(pa uint64) uint64 { return (pa >> 14) << 10 } // offset field, bits 39:10

// Tables maps [Bus, Bus+Size) onto physical [Phys, Phys+Size).
type Tables struct {
	Bus, Phys uint64
	Size      int
	l1        *dma.Buffer
	l2        []*dma.Buffer
}

// Build allocates and fills the page tables (from a, untranslated: the
// DART walks physical memory). Addresses and size must be 16 KB aligned.
func Build(a *dma.Allocator, phys uint64, size int, bus uint64) (*Tables, error) {
	if phys%PageSize != 0 || bus%PageSize != 0 || size%PageSize != 0 || size <= 0 {
		return nil, fmt.Errorf("dart: %#x+%#x at bus %#x is not 16 KB aligned", phys, size, bus)
	}
	if bus+uint64(size) > ttbrSpan {
		return nil, fmt.Errorf("dart: bus range %#x+%#x past one TTBR", bus, size)
	}
	t := &Tables{Bus: bus, Phys: phys, Size: size}
	var err error
	if t.l1, err = a.Alloc(PageSize, PageSize); err != nil {
		return nil, err
	}
	l2s := map[uint64]*dma.Buffer{}
	for off := uint64(0); off < uint64(size); off += PageSize {
		iova := bus + off
		i1, i2 := iova>>25&(entries-1), iova>>14&(entries-1)
		l2 := l2s[i1]
		if l2 == nil {
			if l2, err = a.Alloc(PageSize, PageSize); err != nil {
				t.Free()
				return nil, err
			}
			l2s[i1] = l2
			t.l2 = append(t.l2, l2)
			binary.LittleEndian.PutUint64(t.l1.Bytes[8*i1:], pte(l2.Phys)|PTE_VALID)
		}
		binary.LittleEndian.PutUint64(l2.Bytes[8*i2:], pte(phys+off)|pteSubpageEnd|PTE_VALID)
	}
	return t, nil
}

// Free releases the tables (only once no DART uses them).
func (t *Tables) Free() {
	for _, b := range append(t.l2, t.l1) {
		if b != nil {
			b.Free()
		}
	}
	t.l1, t.l2 = nil, nil
}

// Attach points stream of the DART at r at the tables and turns
// translation on: TTBR 0 valid, the others cleared, the stream enabled,
// its TLB invalidated.
func Attach(r mmio.Regs, stream int, t *Tables) error {
	if stream < 0 || stream >= maxStreams {
		return fmt.Errorf("dart: stream %d", stream)
	}
	if r.Read32(CONFIG)&CONFIG_LOCK != 0 {
		return errors.New("dart: locked by the boot loader")
	}
	ttbr := uint64(TTBR0 + 16*stream)
	r.Write32(TCR0+4*uint64(stream), 0)
	r.Write32(ttbr, TTBR_VALID|uint32(t.l1.Phys>>12)&^TTBR_VALID)
	for i := uint64(1); i < 4; i++ {
		r.Write32(ttbr+4*i, 0)
	}
	r.Write32(TCR0+4*uint64(stream), TCR_TRANSLATE)
	r.Write32(ENABLED_STREAMS, r.Read32(ENABLED_STREAMS)|1<<stream)
	if err := Invalidate(r, stream); err != nil {
		return err
	}
	if e := r.Read32(ERROR); e&ERROR_FLAG != 0 {
		r.Write32(ERROR, e) // a stale fault from before: clear it
	}
	return nil
}

// Invalidate flushes the DART's TLB for stream.
func Invalidate(r mmio.Regs, stream int) error {
	r.Write32(STREAM_SELECT, 1<<stream)
	r.Write32(STREAM_COMMAND, CMD_INVALIDATE)
	deadline := time.Now().Add(invalidateTimout)
	for r.Read32(STREAM_COMMAND)&CMD_BUSY != 0 {
		if time.Now().After(deadline) {
			return errors.New("dart: TLB invalidate timed out")
		}
	}
	return nil
}

// Fault reports the DART's last fault ("" if none): the error code,
// stream and address.
func Fault(r mmio.Regs) string {
	e := r.Read32(ERROR)
	if e&ERROR_FLAG == 0 {
		return ""
	}
	addr := uint64(r.Read32(ERROR_ADDR_HI))<<32 | uint64(r.Read32(ERROR_ADDR_LO))
	return fmt.Sprintf("fault %#x (stream %d) at %#x", e&0xffffff, e>>24&0xf, addr)
}
