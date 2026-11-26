// Package sart programs the t6000 SART (version 3), the address filter in
// front of the SSD coprocessor (ANS2), after m1n1's sart.c: up to 16
// entries, each a physical range the coprocessor may touch. Entries iBoot
// set up are kept; godeep adds one for its DMA region, so the buffers
// RTKit hands the firmware are reachable. The SSD's own data transfers
// aren't filtered by it.
package sart

import (
	"errors"
	"fmt"

	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

// Compatible is the SART in the M1 Pro's device tree.
const Compatible = "apple,t6000-sart"

const (
	CONFIG   = 0x00 // + 4 per entry: flags
	PADDR    = 0x40 // + 4 per entry: address >> 12
	SIZE     = 0x80 // + 4 per entry: size >> 12
	Entries  = 16
	Allow    = 0xff
	sizeMax  = 1<<30 - 1
	pageBits = 12
)

// SART is one filter.
type SART struct {
	r         mmio.Regs
	protected uint16 // entries iBoot set up
}

// New reads which entries are already in use (and must stay).
func New(r mmio.Regs) *SART {
	s := &SART{r: r}
	for i := 0; i < Entries; i++ {
		if r.Read32(CONFIG+4*uint64(i)) != 0 {
			s.protected |= 1 << i
		}
	}
	return s
}

// Allow adds an entry letting the coprocessor reach [base, base+size).
func (s *SART) Allow(base uint64, size int) error {
	if base&(1<<pageBits-1) != 0 || size&(1<<pageBits-1) != 0 || size <= 0 || size>>pageBits > sizeMax {
		return fmt.Errorf("sart: bad range %#x+%#x", base, size)
	}
	for i := 0; i < Entries; i++ {
		o := 4 * uint64(i)
		if s.protected&(1<<i) != 0 || s.r.Read32(CONFIG+o) != 0 {
			continue
		}
		s.r.Write32(PADDR+o, uint32(base>>pageBits))
		s.r.Write32(SIZE+o, uint32(size>>pageBits))
		s.r.Write32(CONFIG+o, Allow)
		return nil
	}
	return errors.New("sart: no free entry")
}

// Clear removes every entry godeep added.
func (s *SART) Clear() {
	for i := 0; i < Entries; i++ {
		if s.protected&(1<<i) == 0 {
			o := 4 * uint64(i)
			s.r.Write32(CONFIG+o, 0)
			s.r.Write32(PADDR+o, 0)
			s.r.Write32(SIZE+o, 0)
		}
	}
}

// Protected is how many entries iBoot had set up.
func (s *SART) Protected() int {
	n := 0
	for i := 0; i < Entries; i++ {
		if s.protected&(1<<i) != 0 {
			n++
		}
	}
	return n
}

// IOMMU is a dma.IOMMU for a coprocessor behind a SART whose entry
// already covers the buffers: the device sees physical addresses.
type IOMMU struct{}

func (IOMMU) Map(b *dma.Buffer, _ dma.Direction) (uint64, error) { return b.Phys, nil }
func (IOMMU) Unmap(uint64, int) error                            { return nil }
