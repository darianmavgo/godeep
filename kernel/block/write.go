package block

import (
	"context"
	"errors"
)

// Writer is a device that can be written. Read-only devices don't
// implement it; a Partition does when its disk does.
type Writer interface {
	Device
	// WriteBlocks writes p (a whole number of blocks) from block lba on.
	WriteBlocks(ctx context.Context, lba uint64, p []byte) error
}

// ErrReadOnly is returned for writes to a device that can't be written.
var ErrReadOnly = errors.New("block: read-only device")

// Writable reports whether d can be written: it implements Writer, and
// doesn't report its medium write-protected (ReadOnly() true).
func Writable(d Device) bool {
	if p, ok := d.(*Partition); ok {
		return Writable(p.parent)
	}
	if _, ok := d.(Writer); !ok {
		return false
	}
	if ro, ok := d.(interface{ ReadOnly() bool }); ok && ro.ReadOnly() {
		return false
	}
	return true
}

// WriteBlocks writes through to the disk (offset by the partition's start).
func (p *Partition) WriteBlocks(ctx context.Context, lba uint64, b []byte) error {
	if err := Check(p, lba, b); err != nil {
		return err
	}
	w, ok := p.parent.(Writer)
	if !ok {
		return ErrReadOnly
	}
	return w.WriteBlocks(ctx, p.Start+lba, b)
}

// WriteAt writes p at byte offset off, through the cache: the blocks it
// touches are read (if not cached), changed and written back, and the cache
// keeps the new contents, so later reads see the write. Implements
// io.WriterAt.
func (r *Reader) WriteAt(p []byte, off int64) (int, error) {
	w, ok := r.d.(Writer)
	if !ok || !Writable(r.d) {
		return 0, ErrReadOnly
	}
	if off < 0 || off+int64(len(p)) > r.Size() {
		return 0, ErrRange
	}
	r.wmu.Lock() // one read-modify-write at a time
	defer r.wmu.Unlock()
	bs := int64(r.d.BlockSize())
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		cn := pos / int64(r.chunk)
		old, err := r.get(cn)
		if err != nil {
			return n, err
		}
		data := append([]byte(nil), old...) // readers may hold the old slice
		in := int(pos % int64(r.chunk))
		m := copy(data[in:], p[n:])
		// write back just the blocks that changed
		b0 := int64(in) / bs * bs
		b1 := (int64(in+m) + bs - 1) / bs * bs
		lba := uint64((cn*int64(r.chunk) + b0) / bs)
		if err := w.WriteBlocks(r.ctx, lba, data[b0:b1]); err != nil {
			return n, err
		}
		r.mu.Lock()
		if e, ok := r.index[cn]; ok {
			e.Value.(*entry).data = data
		}
		r.mu.Unlock()
		n += m
	}
	return n, nil
}
