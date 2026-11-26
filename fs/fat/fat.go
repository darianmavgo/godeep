// Package fat reads FAT12, FAT16 and FAT32 filesystems (with long file
// names) as an io/fs.FS, and writes FAT16 and FAT32 ones (write.go) when
// opened with NewWritable. Names match case-insensitively, as on every
// system that writes FAT.
package fat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"godeep/fs/rofs"
)

// FS is a FAT filesystem.
type FS struct {
	rofs.FS
	r          io.ReaderAt
	bps, spc   uint32 // bytes per sector, sectors per cluster
	fatOff     int64  // first FAT, bytes
	rootOff    int64  // FAT12/16 root directory region, bytes
	rootSize   int64
	rootClus   uint32 // FAT32
	dataOff    int64  // cluster 2, bytes
	clusters   uint32 // data clusters
	bits       int    // 12, 16 or 32
	label      string
	clusterLen int64

	// writing (write.go)
	w         io.WriterAt // nil: read-only
	nfats     int
	fatBytes  int64 // one FAT's size
	fsinfoOff int64 // FAT32 FSInfo sector (0: none)
	freeHint  uint32
	mu        sync.RWMutex // writers exclude readers
	dirty     bool         // free counted (FSInfo maintained from then on)
	free      int64        // free clusters, once dirty
}

// Probe reports whether r starts with a FAT boot sector.
func Probe(r io.ReaderAt) bool {
	_, err := New(r)
	return err == nil
}

// New reads the boot sector (BIOS parameter block).
func New(r io.ReaderAt) (*FS, error) {
	b := make([]byte, 512)
	if _, err := r.ReadAt(b, 0); err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	if b[510] != 0x55 || b[511] != 0xaa || (b[0] != 0xeb && b[0] != 0xe9) {
		return nil, errors.New("fat: no boot sector signature")
	}
	f := &FS{r: r, bps: uint32(le.Uint16(b[11:])), spc: uint32(b[13])}
	rsvd, nfats, rootEnts := uint32(le.Uint16(b[14:])), uint32(b[16]), uint32(le.Uint16(b[17:]))
	tot := uint32(le.Uint16(b[19:]))
	if tot == 0 {
		tot = le.Uint32(b[32:])
	}
	fatSz := uint32(le.Uint16(b[22:]))
	if fatSz == 0 {
		fatSz = le.Uint32(b[36:])
	}
	if f.bps < 512 || f.bps > 4096 || f.bps&(f.bps-1) != 0 || f.spc == 0 || f.spc&(f.spc-1) != 0 ||
		rsvd == 0 || nfats == 0 || fatSz == 0 || tot == 0 {
		return nil, errors.New("fat: invalid BIOS parameter block")
	}
	rootSecs := (rootEnts*32 + f.bps - 1) / f.bps
	meta := rsvd + nfats*fatSz + rootSecs
	if meta >= tot {
		return nil, errors.New("fat: invalid BIOS parameter block")
	}
	f.clusters = (tot - meta) / f.spc
	switch {
	case f.clusters < 4085:
		f.bits = 12
	case f.clusters < 65525:
		f.bits = 16
	default:
		f.bits = 32
	}
	bps := int64(f.bps)
	f.clusterLen = bps * int64(f.spc)
	f.fatOff = int64(rsvd) * bps
	f.rootOff = int64(rsvd+nfats*fatSz) * bps
	f.rootSize = int64(rootEnts) * 32
	f.dataOff = int64(meta) * bps
	f.nfats, f.fatBytes = int(nfats), int64(fatSz)*bps
	labelAt := 43
	if f.bits == 32 {
		f.rootClus = le.Uint32(b[44:])
		labelAt = 71
		if s := le.Uint16(b[48:]); s != 0 && s != 0xffff {
			f.fsinfoOff = int64(s) * bps
		}
	}
	if b[labelAt-5] == 0x29 { // extended boot signature
		f.label = strings.TrimRight(string(b[labelAt:labelAt+11]), " ")
	}
	f.FS = rofs.FS{Root: func() (rofs.Node, error) { return &node{f: f, root: true}, nil }, Fold: true}
	return f, nil
}

// Type is "FAT12", "FAT16" or "FAT32".
func (f *FS) Type() string { return fmt.Sprintf("FAT%d", f.bits) }

// Label is the volume label from the boot sector.
func (f *FS) Label() string { return f.label }

// next returns the cluster after c in its chain (eoc: end of chain).
func (f *FS) next(c uint32) (n uint32, eoc bool, err error) {
	var b [4]byte
	switch f.bits {
	case 12:
		if _, err := f.r.ReadAt(b[:2], f.fatOff+int64(c+c/2)); err != nil {
			return 0, false, err
		}
		n = uint32(binary.LittleEndian.Uint16(b[:]))
		if c&1 == 1 {
			n >>= 4
		}
		n &= 0xfff
		eoc = n >= 0xff8
	case 16:
		if _, err := f.r.ReadAt(b[:2], f.fatOff+int64(c)*2); err != nil {
			return 0, false, err
		}
		n = uint32(binary.LittleEndian.Uint16(b[:]))
		eoc = n >= 0xfff8
	default:
		if _, err := f.r.ReadAt(b[:], f.fatOff+int64(c)*4); err != nil {
			return 0, false, err
		}
		n = binary.LittleEndian.Uint32(b[:]) & 0x0fffffff
		eoc = n >= 0x0ffffff8
	}
	if !eoc && (n < 2 || n >= f.clusters+2) {
		return 0, false, fmt.Errorf("fat: cluster %d points to %#x", c, n)
	}
	return n, eoc, nil
}

// chain lists the clusters of a chain starting at c (0: empty).
func (f *FS) chain(c uint32) ([]uint32, error) {
	var out []uint32
	for c != 0 {
		if c < 2 || c >= f.clusters+2 {
			return nil, fmt.Errorf("fat: bad cluster %d", c)
		}
		out = append(out, c)
		if uint32(len(out)) > f.clusters {
			return nil, errors.New("fat: cluster chain loops")
		}
		n, eoc, err := f.next(c)
		if err != nil {
			return nil, err
		}
		if eoc {
			break
		}
		c = n
	}
	return out, nil
}

func (f *FS) clusterOff(c uint32) int64 { return f.dataOff + int64(c-2)*f.clusterLen }

// node is a directory entry (or the root).
type node struct {
	f       *FS
	root    bool
	name    string
	attr    byte
	cluster uint32
	size    uint32
	mtime   time.Time
	chain   []uint32 // loaded on first read
	entOff  int64    // disk offset of its short directory entry (0: root)
	lfnOffs []int64  // and of its long-name entries
}

const (
	attrVolume = 0x08
	attrDir    = 0x10
	attrLFN    = 0x0f
)

func (n *node) Info() (fs.FileInfo, error) { return info{n}, nil }

func (n *node) isDir() bool { return n.root || n.attr&attrDir != 0 }

func (n *node) clusters() ([]uint32, error) {
	if n.chain == nil {
		c := n.cluster
		if n.root {
			c = n.f.rootClus
		}
		ch, err := n.f.chain(c)
		if err != nil {
			return nil, err
		}
		n.chain = ch
	}
	return n.chain, nil
}

// data reads [off, off+len(p)) of the node's cluster chain.
func (n *node) data(p []byte, off int64) (int, error) {
	ch, err := n.clusters()
	if err != nil {
		return 0, err
	}
	done := 0
	for done < len(p) {
		i := (off + int64(done)) / n.f.clusterLen
		if i >= int64(len(ch)) {
			return done, io.EOF
		}
		in := (off + int64(done)) % n.f.clusterLen
		m := min(int64(len(p)-done), n.f.clusterLen-in)
		if _, err := n.f.r.ReadAt(p[done:done+int(m)], n.f.clusterOff(ch[i])+in); err != nil {
			return done, err
		}
		done += int(m)
	}
	return done, nil
}

func (n *node) ReadAt(p []byte, off int64) (int, error) {
	n.f.mu.RLock()
	defer n.f.mu.RUnlock()
	return n.readAt(p, off)
}

func (n *node) readAt(p []byte, off int64) (int, error) {
	if n.isDir() {
		return 0, errors.New("fat: is a directory")
	}
	if off >= int64(n.size) {
		return 0, io.EOF
	}
	short := false
	if rest := int64(n.size) - off; int64(len(p)) > rest {
		p, short = p[:rest], true
	}
	k, err := n.data(p, off)
	if err == nil && short {
		err = io.EOF
	}
	return k, err
}

func (n *node) Readlink() (string, error) { return "", errors.New("fat: no symlinks") }

func (n *node) ReadDir() ([]rofs.Node, error) {
	n.f.mu.RLock()
	defer n.f.mu.RUnlock()
	ns, err := n.readDir()
	out := make([]rofs.Node, len(ns))
	for i, x := range ns {
		out[i] = x
	}
	return out, err
}

// dirRaw returns a directory's raw entries and where each byte lives on
// the disk.
func (n *node) dirRaw() ([]byte, func(i int) int64, error) {
	if n.root && n.f.bits != 32 {
		raw := make([]byte, n.f.rootSize)
		if _, err := n.f.r.ReadAt(raw, n.f.rootOff); err != nil {
			return nil, nil, err
		}
		return raw, func(i int) int64 { return n.f.rootOff + int64(i) }, nil
	}
	ch, err := n.clusters()
	if err != nil {
		return nil, nil, err
	}
	raw := make([]byte, int64(len(ch))*n.f.clusterLen)
	if _, err := n.data(raw, 0); err != nil && err != io.EOF {
		return nil, nil, err
	}
	return raw, func(i int) int64 {
		return n.f.clusterOff(ch[int64(i)/n.f.clusterLen]) + int64(i)%n.f.clusterLen
	}, nil
}

func (n *node) readDir() ([]*node, error) {
	if !n.isDir() {
		return nil, errors.New("fat: not a directory")
	}
	raw, pos, err := n.dirRaw()
	if err != nil {
		return nil, err
	}
	return n.f.parseDir(raw, pos), nil
}

func (f *FS) parseDir(raw []byte, pos func(int) int64) []*node {
	le := binary.LittleEndian
	var out []*node
	var lfn []uint16
	var lfnAt []int64
	var lfnSum byte
	lfnOK := false
	for i := 0; i+32 <= len(raw); i += 32 {
		e := raw[i : i+32]
		if e[0] == 0 {
			break // end of directory
		}
		if e[0] == 0xe5 {
			lfnOK = false
			continue // deleted
		}
		if e[11]&0x3f == attrLFN {
			seq := int(e[0] & 0x1f)
			if e[0]&0x40 != 0 { // last (first stored) part
				lfn, lfnSum, lfnOK = make([]uint16, 13*seq), e[13], seq > 0
				lfnAt = nil
			}
			lfnAt = append(lfnAt, pos(i))
			if !lfnOK || seq == 0 || 13*seq > len(lfn) || e[13] != lfnSum {
				lfnOK = false
				continue
			}
			at := 13 * (seq - 1)
			for j, o := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
				lfn[at+j] = le.Uint16(e[o:])
			}
			continue
		}
		if e[11]&attrVolume != 0 {
			lfnOK = false
			continue
		}
		name := ""
		var lfnOffs []int64
		if lfnOK && checksum(e[:11]) == lfnSum {
			lfnOffs = lfnAt
			end := 0
			for end < len(lfn) && lfn[end] != 0 && lfn[end] != 0xffff {
				end++
			}
			name = string(utf16.Decode(lfn[:end]))
		} else {
			name = shortName(e)
		}
		lfnOK = false
		if name == "." || name == ".." {
			continue
		}
		cl := uint32(le.Uint16(e[26:]))
		if f.bits == 32 {
			cl |= uint32(le.Uint16(e[20:])) << 16
		}
		out = append(out, &node{f: f, name: name, attr: e[11], cluster: cl, size: le.Uint32(e[28:]),
			mtime: dosTime(le.Uint16(e[24:]), le.Uint16(e[22:])), entOff: pos(i), lfnOffs: lfnOffs})
	}
	return out
}

func checksum(short []byte) byte {
	var s byte
	for _, c := range short {
		s = (s&1)<<7 + s>>1 + c
	}
	return s
}

// shortName formats an 8.3 name, honouring the lower-case flags Windows
// and Linux set in byte 12.
func shortName(e []byte) string {
	base := []byte(strings.TrimRight(string(e[0:8]), " "))
	ext := strings.TrimRight(string(e[8:11]), " ")
	if len(base) > 0 && base[0] == 0x05 {
		base[0] = 0xe5
	}
	b := string(base)
	if e[12]&0x08 != 0 {
		b = strings.ToLower(b)
	}
	if e[12]&0x10 != 0 {
		ext = strings.ToLower(ext)
	}
	if ext != "" {
		return b + "." + ext
	}
	return b
}

func dosTime(d, t uint16) time.Time {
	if d == 0 {
		return time.Time{}
	}
	return time.Date(1980+int(d>>9), time.Month(d>>5&0xf), int(d&0x1f),
		int(t>>11), int(t>>5&0x3f), int(t&0x1f)*2, 0, time.UTC)
}

type info struct{ n *node }

func (i info) Name() string {
	if i.n.root {
		return "."
	}
	return i.n.name
}
func (i info) Size() int64 {
	if i.n.isDir() {
		return 0
	}
	return int64(i.n.size)
}
func (i info) Mode() fs.FileMode {
	if i.n.isDir() {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i info) ModTime() time.Time { return i.n.mtime }
func (i info) IsDir() bool        { return i.n.isDir() }
func (i info) Sys() any           { return nil }
