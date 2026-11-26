package block

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// A Fenced disk can be written only inside the partitions a policy
// allowed when it was fenced: the whole disk reports read-only (so the
// partition table and every other partition are safe), those partitions
// report writable, and every write is checked against them again. The
// Mac's internal SSD, which holds macOS and Omarchy, is fenced to the one
// partition godeep owns (GODEEPDATA).
type Fenced struct {
	w       Writer
	allowed []*Partition
}

// ErrFenced is returned for a write outside the allowed partitions.
var ErrFenced = errors.New("block: write outside the partitions this disk allows")

// Policy decides whether a partition may be written.
type Policy func(ctx context.Context, p *Partition) bool

// Fence reads w's partition table and allows writes to the partitions
// allow accepts (none if it has no table).
func Fence(ctx context.Context, w Writer, allow Policy) (*Fenced, error) {
	f := &Fenced{w: w}
	parts, _, err := Partitions(ctx, w)
	if err != nil && !errors.Is(err, ErrNoTable) {
		return nil, err
	}
	for _, p := range parts {
		if allow(ctx, p) {
			f.allowed = append(f.allowed, p)
		}
	}
	return f, nil
}

func (f *Fenced) BlockSize() int { return f.w.BlockSize() }
func (f *Fenced) Blocks() uint64 { return f.w.Blocks() }
func (f *Fenced) ReadBlocks(ctx context.Context, lba uint64, p []byte) error {
	return f.w.ReadBlocks(ctx, lba, p)
}

// WriteBlocks writes only if the blocks lie inside one allowed partition.
func (f *Fenced) WriteBlocks(ctx context.Context, lba uint64, p []byte) error {
	if err := Check(f, lba, p); err != nil {
		return err
	}
	if !f.CanWrite(lba, uint64(len(p)/f.BlockSize())) {
		return ErrFenced
	}
	return f.w.WriteBlocks(ctx, lba, p)
}

// ReadOnly: the disk as a whole can't be written.
func (f *Fenced) ReadOnly() bool { return true }

// CanWrite reports whether [start, start+count) lies inside one allowed
// partition (and the disk itself is writable).
func (f *Fenced) CanWrite(start, count uint64) bool {
	if count == 0 || !Writable(f.w) {
		return false
	}
	for _, p := range f.allowed {
		if start >= p.Start && start+count <= p.Start+p.Count && start+count > start {
			return true
		}
	}
	return false
}

// Allowed names the writable partitions ("p3 GODEEPDATA").
func (f *Fenced) Allowed() []string {
	var out []string
	for _, p := range f.allowed {
		out = append(out, fmt.Sprintf("p%d %s", p.Index, p.Name))
	}
	return out
}

// Devices passes on the disk's own lsdev lines, plus the fence.
func (f *Fenced) Devices() []string {
	var out []string
	if l, ok := f.w.(interface{ Devices() []string }); ok {
		out = l.Devices()
	}
	fence := "writes fenced: none allowed"
	if len(f.allowed) > 0 {
		fence = "writes only to " + strings.Join(f.Allowed(), ", ")
	}
	return append(out, fence)
}

// FAT32Label reads a FAT32 volume's label from its boot sector ("" if d
// doesn't start with one).
func FAT32Label(ctx context.Context, d Device) string {
	bs := d.BlockSize()
	if bs < 512 || d.Blocks() == 0 {
		return ""
	}
	b := make([]byte, bs)
	if d.ReadBlocks(ctx, 0, b) != nil || b[510] != 0x55 || b[511] != 0xaa || string(b[0x52:0x5a]) != "FAT32   " {
		return ""
	}
	return strings.TrimRight(string(b[0x47:0x52]), " ")
}

// BasicDataFAT32 allows the Microsoft basic data partitions whose FAT32
// label is label (what `diskutil addPartition ... FAT32 GODEEPDATA` makes).
func BasicDataFAT32(label string) Policy {
	return func(ctx context.Context, p *Partition) bool {
		return p.Type == typeNames[TypeBasicData] && FAT32Label(ctx, p) == label
	}
}
