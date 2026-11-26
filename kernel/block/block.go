// Package block is godeep's block layer: the interface disk drivers
// publish (NVMe now; Apple's ANS2, SD cards and USB storage later),
// partition tables (GPT, MBR) and a cached io.ReaderAt that filesystems
// read through. Read-only for now.
package block

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Device is a disk (or a partition of one).
type Device interface {
	BlockSize() int // bytes, a power of two
	Blocks() uint64
	// ReadBlocks fills p (a whole number of blocks) from block lba on.
	ReadBlocks(ctx context.Context, lba uint64, p []byte) error
}

// ErrRange is returned for reads past the end of a device.
var ErrRange = errors.New("block: read past the end of the device")

// Check validates a ReadBlocks request against d.
func Check(d Device, lba uint64, p []byte) error {
	bs := d.BlockSize()
	if len(p)%bs != 0 {
		return fmt.Errorf("block: %d bytes is not a whole number of %d-byte blocks", len(p), bs)
	}
	n := uint64(len(p) / bs)
	if lba > d.Blocks() || n > d.Blocks()-lba {
		return ErrRange
	}
	return nil
}

// Size is the device's size in bytes.
func Size(d Device) int64 { return int64(d.Blocks()) * int64(d.BlockSize()) }

// Reader reads a device at any byte offset, through a cache of recently
// read chunks. It is safe for concurrent use. Reads use the context it was
// made with.
type Reader struct {
	ctx   context.Context
	d     Device
	chunk int // bytes per cache entry, a multiple of the block size

	mu    sync.Mutex
	lru   *list.List // of *entry, most recent first
	index map[int64]*list.Element
	max   int
}

type entry struct {
	n    int64 // chunk number
	data []byte
}

// ChunkSize and CacheChunks size a Reader's cache (64 KB × 64 = 4 MB).
const (
	ChunkSize   = 64 << 10
	CacheChunks = 64
)

// NewReader returns a cached reader of d.
func NewReader(ctx context.Context, d Device) *Reader {
	chunk := ChunkSize
	if bs := d.BlockSize(); bs > chunk {
		chunk = bs
	}
	return &Reader{ctx: ctx, d: d, chunk: chunk, lru: list.New(), index: map[int64]*list.Element{}, max: CacheChunks}
}

// Size is the device size in bytes.
func (r *Reader) Size() int64 { return Size(r.d) }

// ReadAt implements io.ReaderAt.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	size := r.Size()
	if off < 0 {
		return 0, errors.New("block: negative offset")
	}
	if off >= size {
		return 0, io.EOF
	}
	want := len(p)
	if int64(want) > size-off {
		p = p[:size-off]
	}
	n := 0
	for n < len(p) {
		c, err := r.get((off + int64(n)) / int64(r.chunk))
		if err != nil {
			return n, err
		}
		in := int((off + int64(n)) % int64(r.chunk))
		n += copy(p[n:], c[in:])
	}
	if n < want {
		return n, io.EOF
	}
	return n, nil
}

func (r *Reader) get(cn int64) ([]byte, error) {
	r.mu.Lock()
	if e, ok := r.index[cn]; ok {
		r.lru.MoveToFront(e)
		data := e.Value.(*entry).data
		r.mu.Unlock()
		return data, nil
	}
	r.mu.Unlock()

	bs := int64(r.d.BlockSize())
	lba := uint64(cn * int64(r.chunk) / bs)
	nblk := uint64(r.chunk) / uint64(bs)
	if rest := r.d.Blocks() - lba; nblk > rest {
		nblk = rest
	}
	data := make([]byte, nblk*uint64(bs))
	if err := r.d.ReadBlocks(r.ctx, lba, data); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.index[cn]; ok { // read concurrently by someone else
		return e.Value.(*entry).data, nil
	}
	r.index[cn] = r.lru.PushFront(&entry{n: cn, data: data})
	if r.lru.Len() > r.max {
		old := r.lru.Remove(r.lru.Back()).(*entry)
		delete(r.index, old.n)
	}
	return data, nil
}
