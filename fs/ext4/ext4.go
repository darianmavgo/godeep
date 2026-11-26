// Package ext4 reads ext2, ext3 and ext4 filesystems, read-only, as an
// io/fs.FS: extents and block maps (with indirect blocks), 64-bit block
// numbers, hashed (htree) directories (read linearly: the index blocks look
// like empty directory blocks), fast symlinks and small inline-data files.
// The journal isn't replayed: a filesystem that wasn't unmounted cleanly
// is read as it is on disk.
package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"godeep/fs/rofs"
)

// Incompatible features.
const (
	incFiletype   = 0x2
	incRecover    = 0x4
	incJournalDev = 0x8
	incMetaBG     = 0x10
	incExtents    = 0x40
	inc64Bit      = 0x80
	incMMP        = 0x100
	incFlexBG     = 0x200
	incEAInode    = 0x400
	incDirData    = 0x1000
	incCsumSeed   = 0x2000
	incLargeDir   = 0x4000
	incInlineData = 0x8000
	incEncrypt    = 0x10000
	incCasefold   = 0x20000

	supported = incFiletype | incRecover | incExtents | inc64Bit | incMMP | incFlexBG | incEAInode |
		incCsumSeed | incLargeDir | incInlineData | incCasefold
)

// Inode flags.
const (
	flagExtents = 0x80000
	flagInline  = 0x10000000
)

const rootIno = 2

// FS is an ext2/3/4 filesystem.
type FS struct {
	rofs.FS
	r         io.ReaderAt
	bs        int64
	ipg       uint32
	inodeSize int64
	descSize  int64
	groups    uint32
	gdt       []byte
	incompat  uint32
	label     string
	kind      string
}

// Probe reports whether r holds an ext2/3/4 superblock.
func Probe(r io.ReaderAt) bool {
	var b [2]byte
	_, err := r.ReadAt(b[:], 1024+0x38)
	return err == nil && binary.LittleEndian.Uint16(b[:]) == 0xef53
}

// New reads the superblock and group descriptors.
func New(r io.ReaderAt) (*FS, error) {
	sb := make([]byte, 1024)
	if _, err := r.ReadAt(sb, 1024); err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	if le.Uint16(sb[0x38:]) != 0xef53 {
		return nil, errors.New("ext4: no superblock")
	}
	f := &FS{r: r, bs: 1024 << le.Uint32(sb[0x18:]), ipg: le.Uint32(sb[0x28:]), inodeSize: 128, descSize: 32}
	if f.bs > 65536 || f.ipg == 0 {
		return nil, errors.New("ext4: invalid superblock")
	}
	if le.Uint32(sb[0x4c:]) >= 1 { // dynamic revision
		f.inodeSize = int64(le.Uint16(sb[0x58:]))
	}
	compat := le.Uint32(sb[0x5c:])
	f.incompat = le.Uint32(sb[0x60:])
	if bad := f.incompat &^ supported; bad != 0 {
		return nil, fmt.Errorf("ext4: unsupported features %#x", bad)
	}
	if f.incompat&inc64Bit != 0 {
		f.descSize = int64(le.Uint16(sb[0xfe:]))
		if f.descSize < 64 {
			f.descSize = 64
		}
	}
	blocks := uint64(le.Uint32(sb[0x4:]))
	if f.incompat&inc64Bit != 0 {
		blocks |= uint64(le.Uint32(sb[0x150:])) << 32
	}
	first, bpg := uint64(le.Uint32(sb[0x14:])), uint64(le.Uint32(sb[0x20:]))
	if bpg == 0 || blocks <= first || f.inodeSize < 128 {
		return nil, errors.New("ext4: invalid superblock")
	}
	f.groups = uint32((blocks - first + bpg - 1) / bpg)
	f.gdt = make([]byte, int64(f.groups)*f.descSize)
	if _, err := r.ReadAt(f.gdt, int64(first+1)*f.bs); err != nil {
		return nil, fmt.Errorf("ext4: group descriptors: %w", err)
	}
	f.label = strings.TrimRight(string(sb[0x78:0x88]), "\x00")
	switch {
	case f.incompat&(incExtents|inc64Bit|incFlexBG) != 0:
		f.kind = "ext4"
	case compat&0x4 != 0: // has_journal
		f.kind = "ext3"
	default:
		f.kind = "ext2"
	}
	f.FS = rofs.FS{Root: func() (rofs.Node, error) { return f.node(rootIno, ".") }}
	return f, nil
}

// Type is "ext2", "ext3" or "ext4".
func (f *FS) Type() string { return f.kind }

// Label is the volume name.
func (f *FS) Label() string { return f.label }

func (f *FS) node(ino uint32, name string) (*node, error) {
	if ino == 0 || uint64(ino) > uint64(f.groups)*uint64(f.ipg) {
		return nil, fmt.Errorf("ext4: inode %d out of range", ino)
	}
	g, idx := (ino-1)/f.ipg, (ino-1)%f.ipg
	d := f.gdt[int64(g)*f.descSize:]
	le := binary.LittleEndian
	table := uint64(le.Uint32(d[0x8:]))
	if f.descSize >= 64 {
		table |= uint64(le.Uint32(d[0x28:])) << 32
	}
	in := make([]byte, f.inodeSize)
	if _, err := f.r.ReadAt(in, int64(table)*f.bs+int64(idx)*f.inodeSize); err != nil {
		return nil, fmt.Errorf("ext4: inode %d: %w", ino, err)
	}
	return &node{f: f, ino: ino, name: name, in: in}, nil
}

type node struct {
	f    *FS
	ino  uint32
	name string
	in   []byte
}

func (n *node) mode() uint16  { return binary.LittleEndian.Uint16(n.in[0:]) }
func (n *node) flags() uint32 { return binary.LittleEndian.Uint32(n.in[0x20:]) }
func (n *node) size() int64 {
	le := binary.LittleEndian
	return int64(le.Uint32(n.in[0x4:])) | int64(le.Uint32(n.in[0x6c:]))<<32
}
func (n *node) iblock() []byte { return n.in[0x28 : 0x28+60] }

func (n *node) Info() (fs.FileInfo, error) { return info{n}, nil }

// run maps logical block lblk to a physical block and how many blocks
// from there are contiguous (hole: no blocks, read zeros).
func (n *node) run(lblk uint64) (pblk, count uint64, hole bool, err error) {
	if n.flags()&flagExtents != 0 {
		return n.f.extent(n.iblock(), lblk, 0)
	}
	p, err := n.f.blockMap(n.iblock(), lblk)
	return p, 1, p == 0, err
}

// extent searches an extent tree node for lblk.
func (f *FS) extent(b []byte, lblk uint64, depth int) (pblk, count uint64, hole bool, err error) {
	le := binary.LittleEndian
	if depth > 8 || len(b) < 12 || le.Uint16(b) != 0xf30a {
		return 0, 0, false, errors.New("ext4: bad extent tree")
	}
	entries, level := int(le.Uint16(b[2:])), le.Uint16(b[6:])
	if 12+12*entries > len(b) {
		return 0, 0, false, errors.New("ext4: bad extent tree")
	}
	at := func(i int) []byte { return b[12+12*i:] }
	// last entry starting at or before lblk
	i := -1
	for j := 0; j < entries; j++ {
		if uint64(le.Uint32(at(j))) <= lblk {
			i = j
		}
	}
	if level > 0 {
		if i < 0 {
			return 0, 1, true, nil
		}
		e := at(i)
		child := uint64(le.Uint32(e[4:])) | uint64(le.Uint16(e[8:]))<<32
		blk := make([]byte, f.bs)
		if _, err := f.r.ReadAt(blk, int64(child)*f.bs); err != nil {
			return 0, 0, false, err
		}
		return f.extent(blk, lblk, depth+1)
	}
	if i >= 0 {
		e := at(i)
		start, l := uint64(le.Uint32(e)), uint64(le.Uint16(e[4:]))
		uninit := l > 32768
		if uninit {
			l -= 32768
		}
		if lblk < start+l {
			if uninit {
				return 0, start + l - lblk, true, nil // allocated, not written: zeros
			}
			p := uint64(le.Uint32(e[8:])) | uint64(le.Uint16(e[6:]))<<32
			return p + lblk - start, start + l - lblk, false, nil
		}
	}
	// a hole up to the next extent
	if i+1 < entries {
		return 0, uint64(le.Uint32(at(i+1))) - lblk, true, nil
	}
	return 0, 1 << 32, true, nil
}

// blockMap maps lblk through ext2/3 direct and indirect blocks.
func (f *FS) blockMap(ib []byte, lblk uint64) (uint64, error) {
	le := binary.LittleEndian
	per := uint64(f.bs / 4)
	if lblk < 12 {
		return uint64(le.Uint32(ib[4*lblk:])), nil
	}
	lblk -= 12
	for level, span := 1, per; level <= 3; level, span = level+1, span*per {
		if lblk < span {
			blk := uint64(le.Uint32(ib[4*(11+level):]))
			for l := level; l > 0 && blk != 0; l-- {
				span /= per
				var b [4]byte
				if _, err := f.r.ReadAt(b[:], int64(blk)*f.bs+int64(lblk/span*4)); err != nil {
					return 0, err
				}
				blk = uint64(le.Uint32(b[:]))
				lblk %= span
			}
			return blk, nil
		}
		lblk -= span
	}
	return 0, errors.New("ext4: block beyond triple indirect")
}

func (n *node) ReadAt(p []byte, off int64) (int, error) {
	size := n.size()
	if off >= size {
		return 0, io.EOF
	}
	short := false
	if rest := size - off; int64(len(p)) > rest {
		p, short = p[:rest], true
	}
	if n.flags()&flagInline != 0 {
		if size > 60 {
			return 0, errors.New("ext4: inline data beyond the inode isn't supported")
		}
		k := copy(p, n.iblock()[off:])
		if short {
			return k, io.EOF
		}
		return k, nil
	}
	done := 0
	for done < len(p) {
		pos := off + int64(done)
		lblk, in := uint64(pos/n.f.bs), pos%n.f.bs
		pblk, count, hole, err := n.run(lblk)
		if err != nil {
			return done, err
		}
		if count == 0 {
			count = 1
		}
		m := min(int64(len(p)-done), int64(count)*n.f.bs-in)
		chunk := p[done : done+int(m)]
		if hole {
			clear(chunk)
		} else if _, err := n.f.r.ReadAt(chunk, int64(pblk)*n.f.bs+in); err != nil {
			return done, err
		}
		done += int(m)
	}
	if short {
		return done, io.EOF
	}
	return done, nil
}

func (n *node) Readlink() (string, error) {
	size := n.size()
	if size < 60 && n.flags()&flagExtents == 0 || n.flags()&flagInline != 0 {
		if size > 60 {
			return "", errors.New("ext4: bad symlink")
		}
		return string(n.iblock()[:size]), nil // fast symlink
	}
	b := make([]byte, size)
	if _, err := n.ReadAt(b, 0); err != nil && err != io.EOF {
		return "", err
	}
	return string(b), nil
}

func (n *node) ReadDir() ([]rofs.Node, error) {
	if n.mode()&0xf000 != 0x4000 {
		return nil, errors.New("ext4: not a directory")
	}
	raw := make([]byte, n.size())
	if _, err := n.ReadAt(raw, 0); err != nil && err != io.EOF {
		return nil, err
	}
	if n.flags()&flagInline != 0 { // inline directory: parent inode, then entries
		raw = raw[4:]
	}
	le := binary.LittleEndian
	var out []rofs.Node
	for i := 0; i+8 <= len(raw); {
		ino, rec := le.Uint32(raw[i:]), int(le.Uint16(raw[i+4:]))
		nlen := int(raw[i+6])
		if n.f.incompat&incFiletype == 0 {
			nlen = int(le.Uint16(raw[i+6:]))
		}
		if rec < 8 || i+rec > len(raw) || 8+nlen > rec {
			return out, fmt.Errorf("ext4: corrupt directory entry in inode %d", n.ino)
		}
		name := string(raw[i+8 : i+8+nlen])
		i += rec
		if ino == 0 || name == "." || name == ".." {
			continue // free space, a checksum tail, or an htree index block
		}
		c, err := n.f.node(ino, name)
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, nil
}

type info struct{ n *node }

func (i info) Name() string { return i.n.name }
func (i info) Size() int64 {
	if i.IsDir() {
		return 0
	}
	return i.n.size()
}
func (i info) Mode() fs.FileMode {
	m := i.n.mode()
	perm := fs.FileMode(m & 0o777)
	switch m & 0xf000 {
	case 0x4000:
		return fs.ModeDir | perm
	case 0xa000:
		return fs.ModeSymlink | perm
	case 0x8000:
		return perm
	}
	return fs.ModeIrregular | perm // devices, FIFOs, sockets
}
func (i info) ModTime() time.Time {
	return time.Unix(int64(binary.LittleEndian.Uint32(i.n.in[0x10:])), 0).UTC()
}
func (i info) IsDir() bool { return i.n.mode()&0xf000 == 0x4000 }
func (i info) Sys() any    { return nil }
