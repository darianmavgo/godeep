package ext4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"
)

// Writing (NewWritable): the vfs.Writable operations on ext4 filesystems
// with extents, kept consistent for e2fsck -f, without the journal (each
// change is written in place, like ext2: a crash mid-change can leave
// something for fsck to fix, nothing a replay would need).
//
// What it writes:
//   - files: up to 4 contiguous extents in the inode (no extent tree
//     blocks); an overwrite frees the old blocks first. WriteAt and
//     Truncate rewrite the whole file.
//   - directories: rewritten linearly on every change (an htree index is
//     dropped: e2fsck -D can rebuild it), with checksum tails.
//   - bitmaps, group descriptors, superblock counts, all metadata_csum
//     checksums (crc32c); new blocks and inodes only from groups whose
//     bitmaps are initialized.
//
// Filesystems it won't write (Writable reports false): no extents (ext2/3
// block maps), inline data, a journal needing recovery, encryption,
// casefolding, meta_bg, bigalloc, or old-style group checksums (uninit_bg
// without metadata_csum).

const (
	compatROGdtCsum  = 0x10
	compatROBigalloc = 0x200
	compatROMetaCsum = 0x400

	writeOK = incFiletype | incExtents | inc64Bit | incFlexBG | incCsumSeed | incLargeDir | incMMP

	bgInodeUninit = 1
	bgBlockUninit = 2

	flagIndex   = 0x1000
	maxExtents  = 4
	maxExtLen   = 32768
	dirTailSize = 12
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32c is the kernel's raw crc32c (no inversions), which ext4 chains.
func crc32c(c uint32, p []byte) uint32 { return ^crc32.Update(^c, castagnoli, p) }

// ErrNoSpace: not enough free blocks (in initialized groups).
var ErrNoSpace = errors.New("ext4: no space left")

// ErrNotWritable is returned for writes to a filesystem NewWritable can't
// keep consistent (see the package's write notes).
var ErrNotWritable = errors.New("ext4: read-only (unsupported features for writing)")

type writer struct {
	mu       sync.Mutex
	w        io.WriterAt
	sb       []byte
	csum     bool
	seed     uint32
	bpg      uint64
	first    uint64 // first data block
	blocks   uint64
	firstIno uint32
	why      string // "": writable
}

// NewWritable opens an ext4 filesystem for reading and writing.
func NewWritable(r io.ReaderAt, w io.WriterAt) (*FS, error) {
	f, err := New(r)
	if err != nil {
		return nil, err
	}
	sb := make([]byte, 1024)
	if _, err := r.ReadAt(sb, 1024); err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	wr := &writer{w: w, sb: sb, bpg: uint64(le.Uint32(sb[0x20:])), first: uint64(le.Uint32(sb[0x14:])),
		blocks: uint64(le.Uint32(sb[0x4:])), firstIno: 11}
	if f.incompat&inc64Bit != 0 {
		wr.blocks |= uint64(le.Uint32(sb[0x150:])) << 32
	}
	if le.Uint32(sb[0x4c:]) >= 1 {
		wr.firstIno = le.Uint32(sb[0x54:])
	}
	ro := le.Uint32(sb[0x64:])
	wr.csum = ro&compatROMetaCsum != 0
	if wr.csum {
		if f.incompat&incCsumSeed != 0 {
			wr.seed = le.Uint32(sb[0x270:])
		} else {
			wr.seed = crc32c(^uint32(0), sb[0x68:0x78])
		}
	}
	switch {
	case f.incompat&incExtents == 0:
		wr.why = "no extents (ext2/ext3)"
	case f.incompat&^writeOK != 0:
		wr.why = fmt.Sprintf("incompatible features %#x", f.incompat&^writeOK)
	case ro&compatROBigalloc != 0:
		wr.why = "bigalloc"
	case ro&compatROGdtCsum != 0 && !wr.csum:
		wr.why = "uninit_bg group checksums"
	}
	f.wr = wr
	return f, nil
}

// Writable reports whether writes are accepted (vfs.IsWritable).
func (f *FS) Writable() bool { return f.wr != nil && f.wr.why == "" }

func (f *FS) check() error {
	if f.wr == nil {
		return ErrNotWritable
	}
	if f.wr.why != "" {
		return fmt.Errorf("%w: %s", ErrNotWritable, f.wr.why)
	}
	return nil
}

// --- low level ---------------------------------------------------------

func (f *FS) readBlock(b uint64) ([]byte, error) {
	buf := make([]byte, f.bs)
	_, err := f.r.ReadAt(buf, int64(b)*f.bs)
	return buf, err
}

func (f *FS) writeBlock(b uint64, p []byte) error {
	_, err := f.wr.w.WriteAt(p, int64(b)*f.bs)
	return err
}

func (f *FS) desc(g uint32) []byte { return f.gdt[int64(g)*f.descSize : int64(g+1)*f.descSize] }

// field reads a lo/hi split field of a group descriptor.
func (f *FS) field(d []byte, lo, hi int, size int) uint64 {
	le := binary.LittleEndian
	var v uint64
	if size == 4 {
		v = uint64(le.Uint32(d[lo:]))
		if f.descSize >= 64 {
			v |= uint64(le.Uint32(d[hi:])) << 32
		}
	} else {
		v = uint64(le.Uint16(d[lo:]))
		if f.descSize >= 64 {
			v |= uint64(le.Uint16(d[hi:])) << 16
		}
	}
	return v
}

func (f *FS) setField(d []byte, lo, hi int, size int, v uint64) {
	le := binary.LittleEndian
	if size == 4 {
		le.PutUint32(d[lo:], uint32(v))
		if f.descSize >= 64 {
			le.PutUint32(d[hi:], uint32(v>>32))
		}
	} else {
		le.PutUint16(d[lo:], uint16(v))
		if f.descSize >= 64 {
			le.PutUint16(d[hi:], uint16(v>>16))
		}
	}
}

func (f *FS) blockBitmap(g uint32) uint64 { return f.field(f.desc(g), 0x0, 0x20, 4) }
func (f *FS) inodeBitmap(g uint32) uint64 { return f.field(f.desc(g), 0x4, 0x24, 4) }

// flushDesc writes group g's descriptor (with its checksum) to the primary
// table. (Backup copies are only read by fsck when the primary is bad.)
func (f *FS) flushDesc(g uint32) error {
	d := f.desc(g)
	if f.wr.csum {
		binary.LittleEndian.PutUint16(d[0x1e:], 0)
		var gb [4]byte
		binary.LittleEndian.PutUint32(gb[:], g)
		c := crc32c(f.wr.seed, gb[:])
		c = crc32c(c, d)
		binary.LittleEndian.PutUint16(d[0x1e:], uint16(c))
	}
	off := int64(f.wr.first+1)*f.bs + int64(g)*f.descSize
	_, err := f.wr.w.WriteAt(d, off)
	return err
}

func (f *FS) flushSuper() error {
	sb := f.wr.sb
	le := binary.LittleEndian
	le.PutUint32(sb[0x30:], uint32(time.Now().Unix())) // s_wtime
	if f.wr.csum {
		le.PutUint32(sb[0x3fc:], crc32c(^uint32(0), sb[:0x3fc]))
	}
	_, err := f.wr.w.WriteAt(sb, 1024)
	return err
}

// addFree adjusts the superblock's free block and inode counts.
func (f *FS) addFree(blocks, inodes int64) {
	le := binary.LittleEndian
	sb := f.wr.sb
	fb := uint64(le.Uint32(sb[0x0c:])) | uint64(le.Uint32(sb[0x158:]))<<32
	fb = uint64(int64(fb) + blocks)
	le.PutUint32(sb[0x0c:], uint32(fb))
	if f.incompat&inc64Bit != 0 {
		le.PutUint32(sb[0x158:], uint32(fb>>32))
	}
	le.PutUint32(sb[0x10:], uint32(int64(le.Uint32(sb[0x10:]))+inodes))
}

func (f *FS) bitmapCsum(g uint32, bm []byte, inode bool) {
	if !f.wr.csum {
		return
	}
	d := f.desc(g)
	if inode {
		c := crc32c(f.wr.seed, bm[:f.ipg/8])
		binary.LittleEndian.PutUint16(d[0x1a:], uint16(c))
		if f.descSize >= 64 {
			binary.LittleEndian.PutUint16(d[0x3a:], uint16(c>>16))
		}
	} else {
		c := crc32c(f.wr.seed, bm[:f.wr.bpg/8])
		binary.LittleEndian.PutUint16(d[0x18:], uint16(c))
		if f.descSize >= 64 {
			binary.LittleEndian.PutUint16(d[0x38:], uint16(c>>16))
		}
	}
}

// --- blocks ------------------------------------------------------------

type extent struct{ lblk, pblk, n uint64 }

// allocBlocks finds n blocks in at most maxExtents contiguous runs.
func (f *FS) allocBlocks(n uint64) ([]extent, error) {
	var out []extent
	var lblk uint64
	for g := uint32(0); g < f.groups && n > 0; g++ {
		d := f.desc(g)
		if f.field(d, 0x12, 0x12, 2)&bgBlockUninit != 0 || f.field(d, 0x0c, 0x2c, 2) == 0 {
			continue
		}
		bmBlk := f.blockBitmap(g)
		bm, err := f.readBlock(bmBlk)
		if err != nil {
			return nil, err
		}
		base := f.wr.first + uint64(g)*f.wr.bpg
		limit := min(f.wr.bpg, f.wr.blocks-base)
		took := uint64(0)
		for i := uint64(0); i < limit && n > 0; {
			if bm[i/8]&(1<<(i%8)) != 0 {
				i++
				continue
			}
			if len(out) == maxExtents {
				break
			}
			start := i
			for i < limit && n > 0 && i-start < maxExtLen && bm[i/8]&(1<<(i%8)) == 0 {
				bm[i/8] |= 1 << (i % 8)
				i++
				n--
			}
			run := i - start
			out = append(out, extent{lblk: lblk, pblk: base + start, n: run})
			lblk += run
			took += run
		}
		if took == 0 {
			continue
		}
		f.bitmapCsum(g, bm, false)
		if err := f.writeBlock(bmBlk, bm); err != nil {
			return nil, err
		}
		f.setField(d, 0x0c, 0x2c, 2, f.field(d, 0x0c, 0x2c, 2)-took)
		f.addFree(-int64(took), 0)
		if err := f.flushDesc(g); err != nil {
			return nil, err
		}
	}
	if n > 0 {
		f.freeExtents(out) // give back a partial allocation
		if len(out) == maxExtents {
			return nil, errors.New("ext4: free space too fragmented (more than 4 extents)")
		}
		return nil, ErrNoSpace
	}
	return out, nil
}

// freeExtents clears the blocks in the bitmaps.
func (f *FS) freeExtents(es []extent) error {
	for _, e := range es {
		for b := e.pblk; b < e.pblk+e.n; {
			g := uint32((b - f.wr.first) / f.wr.bpg)
			base := f.wr.first + uint64(g)*f.wr.bpg
			bmBlk := f.blockBitmap(g)
			bm, err := f.readBlock(bmBlk)
			if err != nil {
				return err
			}
			freed := uint64(0)
			for ; b < e.pblk+e.n && b < base+f.wr.bpg; b++ {
				i := b - base
				if bm[i/8]&(1<<(i%8)) != 0 {
					bm[i/8] &^= 1 << (i % 8)
					freed++
				}
			}
			f.bitmapCsum(g, bm, false)
			if err := f.writeBlock(bmBlk, bm); err != nil {
				return err
			}
			d := f.desc(g)
			f.setField(d, 0x0c, 0x2c, 2, f.field(d, 0x0c, 0x2c, 2)+freed)
			f.addFree(int64(freed), 0)
			if err := f.flushDesc(g); err != nil {
				return err
			}
		}
	}
	return nil
}

// fileBlocks lists every block a file or directory uses: its data extents
// and, for extent trees, the tree's own blocks.
func (f *FS) fileBlocks(n *node) ([]extent, error) {
	if n.flags()&flagExtents == 0 {
		return nil, errors.New("ext4: not an extent-mapped file")
	}
	var out []extent
	var walk func(b []byte, depth int) error
	walk = func(b []byte, depth int) error {
		le := binary.LittleEndian
		if depth > 8 || len(b) < 12 || le.Uint16(b) != 0xf30a {
			return errors.New("ext4: bad extent tree")
		}
		entries, level := int(le.Uint16(b[2:])), le.Uint16(b[6:])
		for i := 0; i < entries; i++ {
			e := b[12+12*i:]
			if level == 0 {
				l := uint64(le.Uint16(e[4:]))
				if l > maxExtLen {
					l -= maxExtLen
				}
				p := uint64(le.Uint32(e[8:])) | uint64(le.Uint16(e[6:]))<<32
				out = append(out, extent{lblk: uint64(le.Uint32(e)), pblk: p, n: l})
				continue
			}
			child := uint64(le.Uint32(e[4:])) | uint64(le.Uint16(e[8:]))<<32
			out = append(out, extent{pblk: child, n: 1})
			blk, err := f.readBlock(child)
			if err != nil {
				return err
			}
			if err := walk(blk, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return out, walk(n.iblock(), 0)
}

// --- inodes ------------------------------------------------------------

func (f *FS) inodeOffset(ino uint32) int64 {
	g, idx := (ino-1)/f.ipg, (ino-1)%f.ipg
	table := f.field(f.desc(g), 0x8, 0x28, 4)
	return int64(table)*f.bs + int64(idx)*f.inodeSize
}

func (f *FS) writeInode(n *node) error {
	if f.wr.csum {
		le := binary.LittleEndian
		in := n.in
		le.PutUint16(in[0x7c:], 0)
		large := f.inodeSize > 128 && le.Uint16(in[0x80:]) >= 4
		if large {
			le.PutUint16(in[0x82:], 0)
		}
		var b [4]byte
		le.PutUint32(b[:], n.ino)
		c := crc32c(f.wr.seed, b[:])
		le.PutUint32(b[:], le.Uint32(in[0x64:])) // i_generation
		c = crc32c(c, b[:])
		c = crc32c(c, in)
		le.PutUint16(in[0x7c:], uint16(c))
		if large {
			le.PutUint16(in[0x82:], uint16(c>>16))
		}
	}
	_, err := f.wr.w.WriteAt(n.in, f.inodeOffset(n.ino))
	return err
}

// inodeSeed is the per-inode checksum seed (extent tails, dir tails).
func (f *FS) inodeSeed(n *node) uint32 {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], n.ino)
	c := crc32c(f.wr.seed, b[:])
	binary.LittleEndian.PutUint32(b[:], binary.LittleEndian.Uint32(n.in[0x64:]))
	return crc32c(c, b[:])
}

// allocInode takes a free inode and initializes it.
func (f *FS) allocInode(mode uint16) (*node, error) {
	le := binary.LittleEndian
	for g := uint32(0); g < f.groups; g++ {
		d := f.desc(g)
		if f.field(d, 0x12, 0x12, 2)&bgInodeUninit != 0 || f.field(d, 0x0e, 0x2e, 2) == 0 {
			continue
		}
		bmBlk := f.inodeBitmap(g)
		bm, err := f.readBlock(bmBlk)
		if err != nil {
			return nil, err
		}
		for i := uint32(0); i < f.ipg; i++ {
			ino := g*f.ipg + i + 1
			if ino < f.wr.firstIno || bm[i/8]&(1<<(i%8)) != 0 {
				continue
			}
			bm[i/8] |= 1 << (i % 8)
			f.bitmapCsum(g, bm, true)
			if err := f.writeBlock(bmBlk, bm); err != nil {
				return nil, err
			}
			f.setField(d, 0x0e, 0x2e, 2, f.field(d, 0x0e, 0x2e, 2)-1)
			if mode&0xf000 == 0x4000 {
				f.setField(d, 0x10, 0x30, 2, f.field(d, 0x10, 0x30, 2)+1)
			}
			if unused := f.field(d, 0x1c, 0x32, 2); f.ipg-uint32(unused) <= i {
				f.setField(d, 0x1c, 0x32, 2, uint64(f.ipg-i-1))
			}
			f.addFree(0, -1)
			if err := f.flushDesc(g); err != nil {
				return nil, err
			}
			n := &node{f: f, ino: ino, in: make([]byte, f.inodeSize)}
			now := uint32(time.Now().Unix())
			le.PutUint16(n.in[0:], mode)
			for _, off := range []int{0x08, 0x0c, 0x10} {
				le.PutUint32(n.in[off:], now)
			}
			le.PutUint32(n.in[0x20:], flagExtents)
			le.PutUint32(n.in[0x64:], ino*2654435761) // i_generation
			if f.inodeSize > 128 {
				le.PutUint16(n.in[0x80:], 32)  // i_extra_isize
				le.PutUint32(n.in[0x90:], now) // crtime
			}
			setExtents(n, nil)
			return n, nil
		}
	}
	return nil, errors.New("ext4: no free inodes")
}

func (f *FS) freeInode(n *node) error {
	g, i := (n.ino-1)/f.ipg, (n.ino-1)%f.ipg
	bmBlk := f.inodeBitmap(g)
	bm, err := f.readBlock(bmBlk)
	if err != nil {
		return err
	}
	bm[i/8] &^= 1 << (i % 8)
	f.bitmapCsum(g, bm, true)
	if err := f.writeBlock(bmBlk, bm); err != nil {
		return err
	}
	d := f.desc(g)
	f.setField(d, 0x0e, 0x2e, 2, f.field(d, 0x0e, 0x2e, 2)+1)
	if n.mode()&0xf000 == 0x4000 {
		f.setField(d, 0x10, 0x30, 2, f.field(d, 0x10, 0x30, 2)-1)
	}
	f.addFree(0, 1)
	if err := f.flushDesc(g); err != nil {
		return err
	}
	le := binary.LittleEndian
	le.PutUint16(n.in[0x1a:], 0)                         // links
	le.PutUint32(n.in[0x14:], uint32(time.Now().Unix())) // dtime
	return f.writeInode(n)
}

// setExtents puts es (at most maxExtents, depth 0) into the inode.
func setExtents(n *node, es []extent) {
	le := binary.LittleEndian
	ib := n.iblock()
	clear(ib)
	le.PutUint16(ib[0:], 0xf30a)
	le.PutUint16(ib[2:], uint16(len(es)))
	le.PutUint16(ib[4:], maxExtents)
	var blocks uint64
	for i, e := range es {
		x := ib[12+12*i:]
		le.PutUint32(x[0:], uint32(e.lblk))
		le.PutUint16(x[4:], uint16(e.n))
		le.PutUint16(x[6:], uint16(e.pblk>>32))
		le.PutUint32(x[8:], uint32(e.pblk))
		blocks += e.n
	}
	sectors := blocks * uint64(n.f.bs) / 512
	le.PutUint32(n.in[0x1c:], uint32(sectors))
	le.PutUint16(n.in[0x74:], uint16(sectors>>32))
	le.PutUint32(n.in[0x20:], le.Uint32(n.in[0x20:])&^flagIndex|flagExtents)
}

func setSize(n *node, size int64) {
	le := binary.LittleEndian
	le.PutUint32(n.in[0x4:], uint32(size))
	le.PutUint32(n.in[0x6c:], uint32(size>>32))
}

func touch(n *node) {
	now := uint32(time.Now().Unix())
	binary.LittleEndian.PutUint32(n.in[0x0c:], now) // ctime
	binary.LittleEndian.PutUint32(n.in[0x10:], now) // mtime
}

func links(n *node) uint16 { return binary.LittleEndian.Uint16(n.in[0x1a:]) }
func setLinks(n *node, v uint16) {
	binary.LittleEndian.PutUint16(n.in[0x1a:], v)
}

// setData replaces a node's contents: it allocates and writes the new
// blocks first, then frees the old ones, so a full disk leaves the node as
// it was (the inode isn't written).
func (f *FS) setData(n *node, data []byte) error {
	old, err := f.fileBlocks(n)
	if err != nil {
		return err
	}
	need := (uint64(len(data)) + uint64(f.bs) - 1) / uint64(f.bs)
	var es []extent
	if need > 0 {
		if es, err = f.allocBlocks(need); err != nil {
			return err
		}
	}
	for _, e := range es {
		chunk := make([]byte, e.n*uint64(f.bs))
		copy(chunk, data[min(uint64(len(data)), e.lblk*uint64(f.bs)):])
		if _, err := f.wr.w.WriteAt(chunk, int64(e.pblk)*f.bs); err != nil {
			f.freeExtents(es)
			return err
		}
	}
	setExtents(n, es)
	setSize(n, int64(len(data)))
	touch(n)
	return f.freeExtents(old)
}

// discard frees a node created by a change that then failed.
func (f *FS) discard(n *node) {
	if bl, err := f.fileBlocks(n); err == nil {
		f.freeExtents(bl)
	}
	setExtents(n, nil)
	setSize(n, 0)
	f.freeInode(n)
}

// --- directories -------------------------------------------------------

type dirent struct {
	ino  uint32
	typ  byte
	name string
}

func fileType(mode uint16) byte {
	switch mode & 0xf000 {
	case 0x8000:
		return 1
	case 0x4000:
		return 2
	case 0xa000:
		return 7
	case 0x2000:
		return 3
	case 0x6000:
		return 4
	case 0x1000:
		return 5
	case 0xc000:
		return 6
	}
	return 0
}

// entries reads a directory's live entries, "." and ".." first.
func (f *FS) entries(dir *node) ([]dirent, error) {
	if dir.flags()&flagInline != 0 {
		return nil, ErrNotWritable
	}
	raw := make([]byte, dir.size())
	if _, err := dir.ReadAt(raw, 0); err != nil && err != io.EOF {
		return nil, err
	}
	le := binary.LittleEndian
	var out []dirent
	for i := 0; i+8 <= len(raw); {
		ino, rec := le.Uint32(raw[i:]), int(le.Uint16(raw[i+4:]))
		nlen := int(raw[i+6])
		if rec < 8 || i+rec > len(raw) || 8+nlen > rec {
			return nil, fmt.Errorf("ext4: corrupt directory %d", dir.ino)
		}
		if ino != 0 {
			out = append(out, dirent{ino: ino, typ: raw[i+7], name: string(raw[i+8 : i+8+nlen])})
		}
		i += rec
	}
	return out, nil
}

// writeDir stores a directory's entries linearly, in its blocks (more are
// allocated if needed, extras freed), with checksum tails.
func (f *FS) writeDir(dir *node, ents []dirent) error {
	bs := int(f.bs)
	room := bs
	if f.wr.csum {
		room -= dirTailSize
	}
	le := binary.LittleEndian
	var blocks [][]byte
	cur, used, last := []byte(nil), 0, -1
	flush := func() {
		if cur == nil {
			return
		}
		if last >= 0 { // the last entry spans to the tail
			le.PutUint16(cur[last+4:], uint16(room-last))
		}
		blocks = append(blocks, cur)
		cur, used, last = nil, 0, -1
	}
	for _, e := range ents {
		need := (8 + len(e.name) + 3) &^ 3
		if cur != nil && used+need > room {
			flush()
		}
		if cur == nil {
			cur = make([]byte, bs)
		}
		le.PutUint32(cur[used:], e.ino)
		le.PutUint16(cur[used+4:], uint16(need))
		cur[used+6] = byte(len(e.name))
		cur[used+7] = e.typ
		copy(cur[used+8:], e.name)
		last = used
		used += need
	}
	flush()
	data := bytes.Join(blocks, nil)
	if err := f.setData(dir, data); err != nil {
		return err
	}
	if f.wr.csum {
		seed := f.inodeSeed(dir)
		es, err := f.fileBlocks(dir)
		if err != nil {
			return err
		}
		for _, e := range es {
			for k := uint64(0); k < e.n; k++ {
				blk := data[(e.lblk+k)*uint64(bs):][:bs]
				tail := blk[room:]
				clear(tail)
				le.PutUint16(tail[4:], dirTailSize)
				tail[7] = 0xde
				le.PutUint32(tail[8:], crc32c(seed, blk[:room]))
				if err := f.writeBlock(e.pblk+k, blk); err != nil {
					return err
				}
			}
		}
	}
	return f.writeInode(dir)
}

// --- paths -------------------------------------------------------------

// lookup walks to name (no symlinks followed: writes act on links
// themselves).
func (f *FS) lookup(name string) (*node, error) {
	n, err := f.node(rootIno, ".")
	if err != nil {
		return nil, err
	}
	if name == "." {
		return n, nil
	}
	for _, part := range strings.Split(name, "/") {
		if n.mode()&0xf000 != 0x4000 {
			return nil, fs.ErrNotExist
		}
		ents, err := f.entries(n)
		if err != nil {
			return nil, err
		}
		found := false
		for _, e := range ents {
			if e.name == part {
				if n, err = f.node(e.ino, part); err != nil {
					return nil, err
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fs.ErrNotExist
		}
	}
	return n, nil
}

// parent returns name's directory and base name.
func (f *FS) parent(op, name string) (*node, string, error) {
	if !fs.ValidPath(name) || name == "." {
		return nil, "", &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	if len(path.Base(name)) > 255 {
		return nil, "", &fs.PathError{Op: op, Path: name, Err: errors.New("name too long")}
	}
	dir, err := f.lookup(path.Dir(name))
	if err != nil {
		return nil, "", &fs.PathError{Op: op, Path: name, Err: err}
	}
	if dir.mode()&0xf000 != 0x4000 {
		return nil, "", &fs.PathError{Op: op, Path: name, Err: errors.New("not a directory")}
	}
	return dir, path.Base(name), nil
}

func find(ents []dirent, name string) int {
	for i, e := range ents {
		if e.name == name {
			return i
		}
	}
	return -1
}

// --- vfs.Writable -------------------------------------------------------

// WriteFile creates or replaces a regular file.
func (f *FS) WriteFile(name string, data []byte) error {
	if err := f.check(); err != nil {
		return err
	}
	f.wr.mu.Lock()
	defer f.wr.mu.Unlock()
	defer f.flushSuper()
	dir, base, err := f.parent("write", name)
	if err != nil {
		return err
	}
	ents, err := f.entries(dir)
	if err != nil {
		return err
	}
	if i := find(ents, base); i >= 0 {
		n, err := f.node(ents[i].ino, base)
		if err != nil {
			return err
		}
		if n.mode()&0xf000 != 0x8000 {
			return &fs.PathError{Op: "write", Path: name, Err: errors.New("not a regular file")}
		}
		if err := f.setData(n, data); err != nil {
			return err
		}
		return f.writeInode(n)
	}
	n, err := f.allocInode(0x8000 | 0o644)
	if err != nil {
		return err
	}
	setLinks(n, 1)
	if err := f.setData(n, data); err != nil {
		f.discard(n)
		return err
	}
	if err := f.writeInode(n); err != nil {
		return err
	}
	touch(dir)
	if err := f.writeDir(dir, append(ents, dirent{ino: n.ino, typ: 1, name: base})); err != nil {
		f.discard(n)
		return err
	}
	return nil
}

// ReadAt reads the file's current contents.
func (f *FS) ReadAt(name string, p []byte, off int64) (int, error) {
	f.wr.mu.Lock()
	defer f.wr.mu.Unlock()
	n, err := f.lookup(name)
	if err != nil {
		return 0, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	return n.ReadAt(p, off)
}

func (f *FS) contents(name string) ([]byte, error) {
	n, err := f.lookup(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	b := make([]byte, n.size())
	if _, err := n.ReadAt(b, 0); err != nil && err != io.EOF {
		return nil, err
	}
	return b, nil
}

// WriteAt writes into an existing file (rewriting it whole).
func (f *FS) WriteAt(name string, p []byte, off int64) (int, error) {
	if err := f.check(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, fs.ErrInvalid
	}
	f.wr.mu.Lock()
	b, err := f.contents(name)
	f.wr.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if end := off + int64(len(p)); end > int64(len(b)) {
		b = append(b, make([]byte, end-int64(len(b)))...)
	}
	copy(b[off:], p)
	if err := f.WriteFile(name, b); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Truncate changes a file's size.
func (f *FS) Truncate(name string, size int64) error {
	if err := f.check(); err != nil {
		return err
	}
	if size < 0 {
		return fs.ErrInvalid
	}
	f.wr.mu.Lock()
	b, err := f.contents(name)
	f.wr.mu.Unlock()
	if err != nil {
		return err
	}
	if size <= int64(len(b)) {
		b = b[:size]
	} else {
		b = append(b, make([]byte, size-int64(len(b)))...)
	}
	return f.WriteFile(name, b)
}

// Create returns a writer whose contents replace the file on Close.
func (f *FS) Create(name string) (io.WriteCloser, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	if err := f.WriteFile(name, nil); err != nil {
		return nil, err
	}
	return &createFile{f: f, name: name}, nil
}

type createFile struct {
	f    *FS
	name string
	buf  bytes.Buffer
}

func (c *createFile) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *createFile) Close() error                { return c.f.WriteFile(c.name, c.buf.Bytes()) }

// Mkdir makes a directory.
func (f *FS) Mkdir(name string) error {
	if err := f.check(); err != nil {
		return err
	}
	f.wr.mu.Lock()
	defer f.wr.mu.Unlock()
	defer f.flushSuper()
	dir, base, err := f.parent("mkdir", name)
	if err != nil {
		return err
	}
	ents, err := f.entries(dir)
	if err != nil {
		return err
	}
	if find(ents, base) >= 0 {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	n, err := f.allocInode(0x4000 | 0o755)
	if err != nil {
		return err
	}
	setLinks(n, 2)
	if err := f.writeDir(n, []dirent{{n.ino, 2, "."}, {dir.ino, 2, ".."}}); err != nil {
		f.discard(n)
		return err
	}
	setLinks(dir, links(dir)+1)
	touch(dir)
	if err := f.writeDir(dir, append(ents, dirent{ino: n.ino, typ: 2, name: base})); err != nil {
		f.discard(n)
		return err
	}
	return nil
}

// Remove removes a file, a symlink or an empty directory.
func (f *FS) Remove(name string) error {
	if err := f.check(); err != nil {
		return err
	}
	f.wr.mu.Lock()
	defer f.wr.mu.Unlock()
	defer f.flushSuper()
	dir, base, err := f.parent("remove", name)
	if err != nil {
		return err
	}
	ents, err := f.entries(dir)
	if err != nil {
		return err
	}
	i := find(ents, base)
	if i < 0 || base == "." || base == ".." {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	n, err := f.node(ents[i].ino, base)
	if err != nil {
		return err
	}
	isDir := n.mode()&0xf000 == 0x4000
	if isDir {
		sub, err := f.entries(n)
		if err != nil {
			return err
		}
		if len(sub) > 2 {
			return &fs.PathError{Op: "remove", Path: name, Err: errors.New("directory not empty")}
		}
	}
	if err := f.writeDir(dir, append(ents[:i:i], ents[i+1:]...)); err != nil {
		return err
	}
	if isDir {
		setLinks(dir, links(dir)-1)
		touch(dir)
		if err := f.writeInode(dir); err != nil {
			return err
		}
	} else if l := links(n); l > 1 { // other hard links remain
		setLinks(n, l-1)
		return f.writeInode(n)
	}
	if n.mode()&0xf000 != 0xa000 || n.flags()&flagExtents != 0 { // fast symlinks have no blocks
		if bl, err := f.fileBlocks(n); err == nil {
			if err := f.freeExtents(bl); err != nil {
				return err
			}
		}
	}
	setExtents(n, nil)
	setSize(n, 0)
	return f.freeInode(n)
}

// Rename moves an entry. The target must not exist.
func (f *FS) Rename(from, to string) error {
	if err := f.check(); err != nil {
		return err
	}
	f.wr.mu.Lock()
	defer f.wr.mu.Unlock()
	defer f.flushSuper()
	fdir, fbase, err := f.parent("rename", from)
	if err != nil {
		return err
	}
	tdir, tbase, err := f.parent("rename", to)
	if err != nil {
		return err
	}
	fents, err := f.entries(fdir)
	if err != nil {
		return err
	}
	i := find(fents, fbase)
	if i < 0 || fbase == "." || fbase == ".." {
		return &fs.PathError{Op: "rename", Path: from, Err: fs.ErrNotExist}
	}
	e := fents[i]
	if to == from {
		return nil
	}
	if e.typ == 2 && (to == from || strings.HasPrefix(to, from+"/")) {
		return &fs.PathError{Op: "rename", Path: to, Err: errors.New("a directory can't move into itself")}
	}
	if fdir.ino == tdir.ino {
		if find(fents, tbase) >= 0 {
			return &fs.PathError{Op: "rename", Path: to, Err: fs.ErrExist}
		}
		fents[i].name = tbase
		touch(fdir)
		return f.writeDir(fdir, fents)
	}
	tents, err := f.entries(tdir)
	if err != nil {
		return err
	}
	if find(tents, tbase) >= 0 {
		return &fs.PathError{Op: "rename", Path: to, Err: fs.ErrExist}
	}
	if err := f.writeDir(tdir, append(tents, dirent{ino: e.ino, typ: e.typ, name: tbase})); err != nil {
		return err
	}
	if err := f.writeDir(fdir, append(fents[:i:i], fents[i+1:]...)); err != nil {
		f.writeDir(tdir, tents) // undo: the entry stays where it was
		return err
	}
	if e.typ == 2 { // a directory: its ".." and both parents' link counts
		n, err := f.node(e.ino, tbase)
		if err != nil {
			return err
		}
		sub, err := f.entries(n)
		if err != nil {
			return err
		}
		if j := find(sub, ".."); j >= 0 {
			sub[j].ino = tdir.ino
		}
		if err := f.writeDir(n, sub); err != nil {
			return err
		}
		fdir, _ = f.node(fdir.ino, "")
		setLinks(fdir, links(fdir)-1)
		touch(fdir)
		if err := f.writeInode(fdir); err != nil {
			return err
		}
		tdir, _ = f.node(tdir.ino, "")
		setLinks(tdir, links(tdir)+1)
		touch(tdir)
		return f.writeInode(tdir)
	}
	return nil
}
