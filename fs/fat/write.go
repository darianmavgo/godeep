package fat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
	"unicode/utf16"
)

// Writing FAT16 and FAT32 (FAT12 stays read-only: it only appears on tiny
// volumes). Every operation holds f.mu, so readers never see a half-made
// change. Allocation scans the FAT from a hint; new names always get long
// file name entries plus a generated 8.3 alias ("NAME~1.EXT"). On FAT32 the
// FSInfo free-cluster count and next-free hint are kept exact: counted once
// at the first write, then adjusted on every allocation and free.
//
// It implements vfs.Writable: GODEEPDATA (docs/TargetMyMac.md §6.6) becomes
// a place godeep saves files that macOS and Omarchy read.

// NewWritable opens a FAT filesystem that can be written through w (the
// same device as r: a block.Reader is both).
func NewWritable(r io.ReaderAt, w io.WriterAt) (*FS, error) {
	f, err := New(r)
	if err != nil {
		return nil, err
	}
	if f.bits == 12 {
		return f, nil // stays read-only
	}
	f.w = w
	f.freeHint = 2
	return f, nil
}

// Writable reports whether writes are possible.
func (f *FS) Writable() bool { return f.w != nil }

// ErrReadOnly is returned by writes to a read-only FAT.
var ErrReadOnly = errors.New("fat: read-only")

const (
	fat16EOC = 0xffff
	fat32EOC = 0x0fffffff
)

// ---- the FAT ----

func (f *FS) entryOff(c uint32, copyN int) int64 {
	if f.bits == 16 {
		return f.fatOff + int64(copyN)*f.fatBytes + int64(c)*2
	}
	return f.fatOff + int64(copyN)*f.fatBytes + int64(c)*4
}

// raw reads a FAT entry (0: free).
func (f *FS) raw(c uint32) (uint32, error) {
	var b [4]byte
	if f.bits == 16 {
		if _, err := f.r.ReadAt(b[:2], f.entryOff(c, 0)); err != nil {
			return 0, err
		}
		return uint32(binary.LittleEndian.Uint16(b[:])), nil
	}
	if _, err := f.r.ReadAt(b[:], f.entryOff(c, 0)); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]) & 0x0fffffff, nil
}

// set writes a FAT entry in every copy of the FAT.
func (f *FS) set(c, v uint32) error {
	for k := 0; k < f.nfats; k++ {
		var b [4]byte
		if f.bits == 16 {
			binary.LittleEndian.PutUint16(b[:], uint16(v))
			if _, err := f.w.WriteAt(b[:2], f.entryOff(c, k)); err != nil {
				return err
			}
			continue
		}
		if _, err := f.r.ReadAt(b[:], f.entryOff(c, k)); err != nil {
			return err
		}
		old := binary.LittleEndian.Uint32(b[:])
		binary.LittleEndian.PutUint32(b[:], old&0xf0000000|v&0x0fffffff) // keep the reserved bits
		if _, err := f.w.WriteAt(b[:], f.entryOff(c, k)); err != nil {
			return err
		}
	}
	return nil
}

func (f *FS) eoc() uint32 {
	if f.bits == 16 {
		return fat16EOC
	}
	return fat32EOC
}

// ErrFull is returned when no free cluster is left.
var ErrFull = errors.New("fat: filesystem full")

// alloc takes a free cluster, marks it end-of-chain and zeroes it.
func (f *FS) alloc() (uint32, error) {
	if err := f.invalidateFSInfo(); err != nil {
		return 0, err
	}
	for i := uint32(0); i < f.clusters; i++ {
		c := 2 + (f.freeHint-2+i)%f.clusters
		v, err := f.raw(c)
		if err != nil {
			return 0, err
		}
		if v != 0 {
			continue
		}
		if err := f.set(c, f.eoc()); err != nil {
			return 0, err
		}
		f.freeHint = c + 1
		if f.freeHint >= f.clusters+2 {
			f.freeHint = 2
		}
		if err := f.adjustFree(-1); err != nil {
			return 0, err
		}
		return c, f.zero(c)
	}
	return 0, ErrFull
}

func (f *FS) zero(c uint32) error {
	_, err := f.w.WriteAt(make([]byte, f.clusterLen), f.clusterOff(c))
	return err
}

// freeChain releases a chain (start 0: nothing).
func (f *FS) freeChain(start uint32) error {
	ch, err := f.chain(start)
	if err != nil {
		return err
	}
	if len(ch) > 0 {
		if err := f.invalidateFSInfo(); err != nil {
			return err
		}
	}
	for _, c := range ch {
		if err := f.set(c, 0); err != nil {
			return err
		}
	}
	return f.adjustFree(int64(len(ch)))
}

// invalidateFSInfo makes sure the free count is known before it changes
// (counting the FAT once), so adjustFree can keep FSInfo exact.
func (f *FS) invalidateFSInfo() error {
	if f.dirty || f.fsinfoOff == 0 {
		return nil
	}
	free := int64(0)
	for c := uint32(2); c < f.clusters+2; c++ {
		v, err := f.raw(c)
		if err != nil {
			return err
		}
		if v == 0 {
			free++
		}
	}
	f.free, f.dirty = free, true
	return nil
}

// adjustFree changes the free count by d and writes FSInfo (FAT32).
func (f *FS) adjustFree(d int64) error {
	if f.fsinfoOff == 0 {
		return nil
	}
	f.free += d
	var b [8]byte
	binary.LittleEndian.PutUint32(b[0:], uint32(f.free))
	binary.LittleEndian.PutUint32(b[4:], f.freeHint)
	_, err := f.w.WriteAt(b[:], f.fsinfoOff+488)
	return err
}

// ---- nodes and entries ----

// lookup finds a path's node and its parent ("." is the root, no parent).
func (f *FS) lookup(name string) (parent, n *node, err error) {
	root := &node{f: f, root: true}
	if name == "." {
		return nil, root, nil
	}
	cur := root
	parts := strings.Split(name, "/")
	for i, p := range parts {
		kids, err := cur.readDir()
		if err != nil {
			return nil, nil, err
		}
		var found *node
		for _, k := range kids {
			if strings.EqualFold(k.name, p) {
				found = k
				break
			}
		}
		if found == nil {
			if i == len(parts)-1 {
				return cur, nil, fs.ErrNotExist
			}
			return nil, nil, fs.ErrNotExist
		}
		if i < len(parts)-1 {
			if !found.isDir() {
				return nil, nil, errors.New("not a directory")
			}
			cur = found
			continue
		}
		return cur, found, nil
	}
	return nil, nil, fs.ErrNotExist
}

// dirCluster is the cluster a ".." entry names for a directory node.
func (n *node) dirCluster() uint32 {
	if n.root {
		return 0
	}
	return n.cluster
}

func (f *FS) writeEntryField(off int64, at int, b []byte) error {
	_, err := f.w.WriteAt(b, off+int64(at))
	return err
}

// update rewrites a node's cluster, size and modification time in its
// directory entry.
func (f *FS) update(n *node) error {
	e := make([]byte, 32)
	if _, err := f.r.ReadAt(e, n.entOff); err != nil {
		return err
	}
	le := binary.LittleEndian
	le.PutUint16(e[26:], uint16(n.cluster))
	le.PutUint16(e[20:], uint16(n.cluster>>16))
	le.PutUint32(e[28:], n.size)
	d, t := dosStamp(time.Now())
	le.PutUint16(e[22:], t)
	le.PutUint16(e[24:], d)
	le.PutUint16(e[18:], d) // access date
	_, err := f.w.WriteAt(e, n.entOff)
	return err
}

func dosStamp(t time.Time) (date, tm uint16) {
	t = t.UTC()
	if t.Year() < 1980 {
		t = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	date = uint16(t.Year()-1980)<<9 | uint16(t.Month())<<5 | uint16(t.Day())
	tm = uint16(t.Hour())<<11 | uint16(t.Minute())<<5 | uint16(t.Second()/2)
	return
}

// validName rejects names FAT can't store.
func validName(n string) error {
	if n == "" || n == "." || n == ".." || len(n) > 255 || strings.ContainsAny(n, `"*/:<>?\|`) {
		return fmt.Errorf("fat: bad name %q", n)
	}
	for _, r := range n {
		if r < 0x20 {
			return fmt.Errorf("fat: bad name %q", n)
		}
	}
	return nil
}

// shortAlias makes a unique 8.3 alias "BASE~N.EXT" for a long name.
func shortAlias(long string, taken map[string]bool) [11]byte {
	clean := func(s string, max int) string {
		var b []byte
		for _, r := range strings.ToUpper(s) {
			switch {
			case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("!#$%&'()-@^_`{}~", r):
				b = append(b, byte(r))
			}
			if len(b) == max {
				break
			}
		}
		return string(b)
	}
	base, ext := long, ""
	if i := strings.LastIndexByte(long, '.'); i > 0 {
		base, ext = long[:i], long[i+1:]
	}
	b, e := clean(base, 8), clean(ext, 3)
	if b == "" {
		b = "FILE"
	}
	var out [11]byte
	for n := 1; ; n++ {
		tail := fmt.Sprintf("~%d", n)
		stem := b
		if len(stem)+len(tail) > 8 {
			stem = stem[:8-len(tail)]
		}
		copy(out[:], fmt.Sprintf("%-8s%-3s", stem+tail, e))
		if !taken[string(out[:])] {
			return out
		}
	}
}

// entries builds the long-name entries and the short entry for a name.
func entries(name string, short [11]byte, attr byte, cluster, size uint32) [][]byte {
	le := binary.LittleEndian
	u := utf16.Encode([]rune(name))
	nparts := (len(u) + 12) / 13
	sum := checksum(short[:])
	var out [][]byte
	for seq := nparts; seq >= 1; seq-- {
		e := make([]byte, 32)
		e[0] = byte(seq)
		if seq == nparts {
			e[0] |= 0x40
		}
		e[11], e[13] = attrLFN, sum
		for j, o := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
			k := 13*(seq-1) + j
			v := uint16(0xffff)
			switch {
			case k < len(u):
				v = u[k]
			case k == len(u):
				v = 0
			}
			le.PutUint16(e[o:], v)
		}
		out = append(out, e)
	}
	e := make([]byte, 32)
	copy(e, short[:])
	e[11] = attr
	d, t := dosStamp(time.Now())
	le.PutUint16(e[14:], t)
	le.PutUint16(e[16:], d)
	le.PutUint16(e[18:], d)
	le.PutUint16(e[22:], t)
	le.PutUint16(e[24:], d)
	le.PutUint16(e[20:], uint16(cluster>>16))
	le.PutUint16(e[26:], uint16(cluster))
	le.PutUint32(e[28:], size)
	return append(out, e)
}

// addEntry writes a new entry (with long name) into directory dir and
// returns its node.
func (f *FS) addEntry(dir *node, name string, attr byte, cluster, size uint32) (*node, error) {
	raw, pos, err := dir.dirRaw()
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for i := 0; i+32 <= len(raw); i += 32 {
		if raw[i] != 0 && raw[i] != 0xe5 && raw[i+11]&0x3f != attrLFN {
			taken[string(raw[i:i+11])] = true
		}
	}
	ents := entries(name, shortAlias(name, taken), attr, cluster, size)
	need := len(ents)
	// a run of free slots (deleted, or past the end marker)
	start, run := -1, 0
	for i := 0; i+32 <= len(raw); i += 32 {
		if raw[i] == 0 || raw[i] == 0xe5 {
			if run == 0 {
				start = i
			}
			run++
			if run == need {
				break
			}
		} else {
			run = 0
		}
	}
	if run < need {
		if dir.root && f.bits != 32 {
			return nil, errors.New("fat: root directory full")
		}
		// grow the directory by a (zeroed) cluster
		c, err := f.alloc()
		if err != nil {
			return nil, err
		}
		ch, _ := dir.clusters()
		if err := f.set(ch[len(ch)-1], c); err != nil {
			return nil, err
		}
		dir.chain = nil
		if raw, pos, err = dir.dirRaw(); err != nil {
			return nil, err
		}
		if run == 0 {
			start = len(raw) - int(f.clusterLen)
		}
	}
	var offs []int64
	for k, e := range ents {
		off := pos(start + 32*k)
		if _, err := f.w.WriteAt(e, off); err != nil {
			return nil, err
		}
		offs = append(offs, off)
	}
	return &node{f: f, name: name, attr: attr, cluster: cluster, size: size, mtime: time.Now(),
		entOff: offs[len(offs)-1], lfnOffs: offs[:len(offs)-1]}, nil
}

// delEntry marks a node's entries deleted.
func (f *FS) delEntry(n *node) error {
	for _, off := range append(append([]int64(nil), n.lfnOffs...), n.entOff) {
		if _, err := f.w.WriteAt([]byte{0xe5}, off); err != nil {
			return err
		}
	}
	return nil
}

// ---- data ----

// grow makes n's chain cover size bytes.
func (f *FS) grow(n *node, size int64) error {
	ch, err := n.clusters()
	if err != nil {
		return err
	}
	want := int((size + f.clusterLen - 1) / f.clusterLen)
	for len(ch) < want {
		c, err := f.alloc()
		if err != nil {
			return err
		}
		if len(ch) == 0 {
			n.cluster = c
		} else if err := f.set(ch[len(ch)-1], c); err != nil {
			return err
		}
		ch = append(ch, c)
	}
	n.chain = ch
	return nil
}

func (f *FS) writeData(n *node, p []byte, off int64) (int, error) {
	end := off + int64(len(p))
	if end > 0xffffffff {
		return 0, errors.New("fat: files are limited to 4 GB")
	}
	if err := f.grow(n, end); err != nil {
		return 0, err
	}
	// zero any gap between the old end and off (the clusters past the old
	// end were zeroed when allocated, but the old last cluster wasn't)
	if old := int64(n.size); off > old {
		gap := make([]byte, min(off-old, f.clusterLen-old%f.clusterLen))
		if old%f.clusterLen != 0 {
			if _, err := f.writeAt(n, gap, old); err != nil {
				return 0, err
			}
		}
	}
	k, err := f.writeAt(n, p, off)
	if end > int64(n.size) {
		n.size = uint32(end)
	}
	if uerr := f.update(n); err == nil {
		err = uerr
	}
	return k, err
}

// writeAt writes into n's existing chain.
func (f *FS) writeAt(n *node, p []byte, off int64) (int, error) {
	done := 0
	for done < len(p) {
		i := (off + int64(done)) / f.clusterLen
		in := (off + int64(done)) % f.clusterLen
		m := min(int64(len(p)-done), f.clusterLen-in)
		if _, err := f.w.WriteAt(p[done:done+int(m)], f.clusterOff(n.chain[i])+in); err != nil {
			return done, err
		}
		done += int(m)
	}
	return done, nil
}

// ---- vfs.Writable ----

func (f *FS) begin(op, name string) error {
	if f.w == nil {
		return &fs.PathError{Op: op, Path: name, Err: ErrReadOnly}
	}
	if !fs.ValidPath(name) || name == "." {
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	return nil
}

// file finds an existing regular file (or creates it with create).
func (f *FS) file(op, name string, create bool) (*node, error) {
	parent, n, err := f.lookup(name)
	if err == nil {
		if n.isDir() {
			return nil, &fs.PathError{Op: op, Path: name, Err: errors.New("is a directory")}
		}
		return n, nil
	}
	if !errors.Is(err, fs.ErrNotExist) || parent == nil || !create {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	base := path.Base(name)
	if err := validName(base); err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	n, err = f.addEntry(parent, base, 0x20, 0, 0) // archive
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	return n, nil
}

// WriteFile creates or replaces a file.
func (f *FS) WriteFile(name string, data []byte) error {
	if err := f.begin("write", name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.file("write", name, true)
	if err != nil {
		return err
	}
	if err := f.truncate(n, 0); err != nil {
		return err
	}
	_, err = f.writeData(n, data, 0)
	return err
}

// Create returns a writer for a new (or emptied) file.
func (f *FS) Create(name string) (io.WriteCloser, error) {
	if err := f.WriteFile(name, nil); err != nil {
		return nil, err
	}
	return &writer{f: f, name: name}, nil
}

type writer struct {
	f    *FS
	name string
	off  int64
}

func (w *writer) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(w.name, p, w.off)
	w.off += int64(n)
	return n, err
}

func (w *writer) Close() error { return nil }

// WriteAt writes into an existing file, growing it as needed.
func (f *FS) WriteAt(name string, p []byte, off int64) (int, error) {
	if err := f.begin("write", name); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.file("write", name, false)
	if err != nil {
		return 0, err
	}
	return f.writeData(n, p, off)
}

// ReadAt reads a file's current contents.
func (f *FS) ReadAt(name string, p []byte, off int64) (int, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, n, err := f.lookup(name)
	if err != nil {
		return 0, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	return n.readAt(p, off)
}

func (f *FS) truncate(n *node, size int64) error {
	switch {
	case size == 0:
		if err := f.freeChain(n.cluster); err != nil {
			return err
		}
		n.cluster, n.size, n.chain = 0, 0, nil
	case size < int64(n.size):
		ch, err := n.clusters()
		if err != nil {
			return err
		}
		keep := int((size + f.clusterLen - 1) / f.clusterLen)
		if keep < len(ch) {
			if err := f.freeChain(ch[keep]); err != nil {
				return err
			}
			if err := f.set(ch[keep-1], f.eoc()); err != nil {
				return err
			}
			n.chain = ch[:keep]
		}
		n.size = uint32(size)
	case size > int64(n.size):
		if _, err := f.writeData(n, make([]byte, size-int64(n.size)), int64(n.size)); err != nil {
			return err
		}
	}
	return f.update(n)
}

// Truncate sets a file's size.
func (f *FS) Truncate(name string, size int64) error {
	if err := f.begin("truncate", name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.file("truncate", name, false)
	if err != nil {
		return err
	}
	return f.truncate(n, size)
}

// Mkdir makes a directory (with its "." and ".." entries).
func (f *FS) Mkdir(name string) error {
	if err := f.begin("mkdir", name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	parent, n, err := f.lookup(name)
	if err == nil && n != nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	if parent == nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrNotExist}
	}
	if err := validName(path.Base(name)); err != nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: err}
	}
	c, err := f.alloc()
	if err != nil {
		return err
	}
	dots := append(entries(".", [11]byte{'.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}, attrDir, c, 0)[1],
		entries("..", [11]byte{'.', '.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}, attrDir, parent.dirCluster(), 0)[1]...)
	if _, err := f.w.WriteAt(dots, f.clusterOff(c)); err != nil {
		return err
	}
	_, err = f.addEntry(parent, path.Base(name), attrDir, c, 0)
	return err
}

// Remove removes a file or an empty directory.
func (f *FS) Remove(name string) error {
	if err := f.begin("remove", name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, n, err := f.lookup(name)
	if err != nil {
		return &fs.PathError{Op: "remove", Path: name, Err: err}
	}
	if n.isDir() {
		kids, err := n.readDir()
		if err != nil {
			return err
		}
		if len(kids) > 0 {
			return &fs.PathError{Op: "remove", Path: name, Err: errors.New("directory not empty")}
		}
	}
	if err := f.delEntry(n); err != nil {
		return err
	}
	return f.freeChain(n.cluster)
}

// Rename moves a file or directory, replacing a file at the target.
func (f *FS) Rename(from, to string) error {
	if err := f.begin("rename", from); err != nil {
		return err
	}
	if err := f.begin("rename", to); err != nil {
		return err
	}
	if strings.HasPrefix(strings.ToLower(to)+"/", strings.ToLower(from)+"/") {
		return &fs.PathError{Op: "rename", Path: from, Err: fs.ErrInvalid}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	oldParent, n, err := f.lookup(from)
	if err != nil {
		return &fs.PathError{Op: "rename", Path: from, Err: err}
	}
	newParent, existing, err := f.lookup(to)
	switch {
	case err == nil && existing != nil:
		if existing.isDir() {
			return &fs.PathError{Op: "rename", Path: to, Err: fs.ErrExist}
		}
		if err := f.delEntry(existing); err != nil {
			return err
		}
		if err := f.freeChain(existing.cluster); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist) || newParent == nil:
		return &fs.PathError{Op: "rename", Path: to, Err: err}
	}
	if err := validName(path.Base(to)); err != nil {
		return &fs.PathError{Op: "rename", Path: to, Err: err}
	}
	if err := f.delEntry(n); err != nil {
		return err
	}
	if _, err := f.addEntry(newParent, path.Base(to), n.attr, n.cluster, n.size); err != nil {
		return err
	}
	// a directory that changed parents: its ".." follows
	if n.isDir() && oldParent.dirCluster() != newParent.dirCluster() && n.cluster != 0 {
		var b [2]byte
		le := binary.LittleEndian
		dot := f.clusterOff(n.cluster) + 32 // the ".." entry
		le.PutUint16(b[:], uint16(newParent.dirCluster()))
		if err := f.writeEntryField(dot, 26, b[:]); err != nil {
			return err
		}
		le.PutUint16(b[:], uint16(newParent.dirCluster()>>16))
		if err := f.writeEntryField(dot, 20, b[:]); err != nil {
			return err
		}
	}
	return nil
}
