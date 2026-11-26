// Package memfs is a writable filesystem in memory (the shell's "ram:"
// volume): where files pushed over SSH (scp, sftp) land, and where WASM
// programs are run from. Its contents are lost at reboot.
//
// It implements io/fs.FS (and fs.StatFS, fs.ReadDirFS) for reading, plus
// the write operations in vfs.Writable. Safe for concurrent use. Total file
// data is capped (Limit) so a runaway upload can't eat the heap.
package memfs

import (
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultLimit caps the total size of file data.
const DefaultLimit = 512 << 20

// ErrFull is returned when a write would exceed the limit.
var ErrFull = errors.New("memfs: out of space")

type node struct {
	name    string
	dir     bool
	data    []byte
	kids    map[string]*node
	modTime time.Time
	mode    fs.FileMode
}

// FS is an in-memory filesystem.
type FS struct {
	mu    sync.RWMutex
	root  *node
	used  int64
	Limit int64
	label string
}

// New returns an empty filesystem.
func New(label string) *FS {
	return &FS{root: &node{name: ".", dir: true, kids: map[string]*node{}, modTime: time.Now(), mode: fs.ModeDir | 0o755},
		Limit: DefaultLimit, label: label}
}

func (f *FS) Type() string  { return "memfs" }
func (f *FS) Label() string { return f.label }

// Used is the total size of file data.
func (f *FS) Used() int64 { f.mu.RLock(); defer f.mu.RUnlock(); return f.used }

// clean checks an io/fs path ("a/b", "." for the root; no leading slash).
func clean(op, name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	return name, nil
}

// walk finds the node at name (cleaned).
func (f *FS) walk(name string) (*node, error) {
	n := f.root
	if name == "." {
		return n, nil
	}
	for _, part := range strings.Split(name, "/") {
		if !n.dir {
			return nil, fs.ErrNotExist
		}
		k := n.kids[part]
		if k == nil {
			return nil, fs.ErrNotExist
		}
		n = k
	}
	return n, nil
}

// parent returns the directory that holds name, and name's last element.
func (f *FS) parent(op, name string) (*node, string, error) {
	dir, base := path.Split(name)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "."
	}
	d, err := f.walk(dir)
	if err != nil {
		return nil, "", &fs.PathError{Op: op, Path: name, Err: err}
	}
	if !d.dir {
		return nil, "", &fs.PathError{Op: op, Path: name, Err: errors.New("not a directory")}
	}
	return d, base, nil
}

// ---- reading (io/fs) ----

func (f *FS) Open(name string) (fs.File, error) {
	name, err := clean("open", name)
	if err != nil {
		return nil, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	n, err := f.walk(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	if n.dir {
		return &dirFile{info: n.info(), ents: n.entries()}, nil
	}
	return &readFile{info: n.info(), r: strings.NewReader(string(n.data))}, nil
}

func (f *FS) Stat(name string) (fs.FileInfo, error) {
	name, err := clean("stat", name)
	if err != nil {
		return nil, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	n, err := f.walk(name)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return n.info(), nil
}

func (f *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	name, err := clean("readdir", name)
	if err != nil {
		return nil, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	n, err := f.walk(name)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	if !n.dir {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	return n.entries(), nil
}

type info struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
}

func (i info) Name() string               { return i.name }
func (i info) Size() int64                { return i.size }
func (i info) Mode() fs.FileMode          { return i.mode }
func (i info) ModTime() time.Time         { return i.modTime }
func (i info) IsDir() bool                { return i.mode.IsDir() }
func (i info) Sys() any                   { return nil }
func (i info) Type() fs.FileMode          { return i.mode.Type() }
func (i info) Info() (fs.FileInfo, error) { return i, nil }

func (n *node) info() info {
	return info{name: n.name, size: int64(len(n.data)), mode: n.mode, modTime: n.modTime}
}

func (n *node) entries() []fs.DirEntry {
	out := make([]fs.DirEntry, 0, len(n.kids))
	for _, k := range n.kids {
		out = append(out, k.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

type readFile struct {
	info info
	r    *strings.Reader
}

func (r *readFile) Stat() (fs.FileInfo, error)                { return r.info, nil }
func (r *readFile) Read(p []byte) (int, error)                { return r.r.Read(p) }
func (r *readFile) ReadAt(p []byte, off int64) (int, error)   { return r.r.ReadAt(p, off) }
func (r *readFile) Seek(off int64, whence int) (int64, error) { return r.r.Seek(off, whence) }
func (r *readFile) Close() error                              { return nil }

type dirFile struct {
	info info
	ents []fs.DirEntry
	pos  int
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *dirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: errors.New("is a directory")}
}
func (d *dirFile) Close() error { return nil }
func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.ents[d.pos:]
	if n <= 0 {
		d.pos = len(d.ents)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	rest = rest[:min(n, len(rest))]
	d.pos += len(rest)
	return rest, nil
}

// ---- writing ----

// WriteFile creates or replaces a file.
func (f *FS) WriteFile(name string, data []byte) error {
	name, err := clean("write", name)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, base, err := f.parent("write", name)
	if err != nil {
		return err
	}
	old := d.kids[base]
	if old != nil && old.dir {
		return &fs.PathError{Op: "write", Path: name, Err: errors.New("is a directory")}
	}
	oldSize := int64(0)
	if old != nil {
		oldSize = int64(len(old.data))
	}
	if f.used-oldSize+int64(len(data)) > f.Limit {
		return &fs.PathError{Op: "write", Path: name, Err: ErrFull}
	}
	f.used += int64(len(data)) - oldSize
	now := time.Now()
	d.kids[base] = &node{name: base, data: append([]byte(nil), data...), modTime: now, mode: 0o644}
	d.modTime = now
	return nil
}

// Create returns a writer for a new (or truncated) file; the data appears
// as it is written and is complete once Close returns.
func (f *FS) Create(name string) (io.WriteCloser, error) {
	if err := f.WriteFile(name, nil); err != nil {
		return nil, err
	}
	name, _ = clean("create", name)
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

// WriteAt writes p at off in an existing file, growing it as needed.
func (f *FS) WriteAt(name string, p []byte, off int64) (int, error) {
	name, err := clean("write", name)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.walk(name)
	if err != nil {
		return 0, &fs.PathError{Op: "write", Path: name, Err: err}
	}
	if n.dir {
		return 0, &fs.PathError{Op: "write", Path: name, Err: errors.New("is a directory")}
	}
	end := off + int64(len(p))
	if grow := end - int64(len(n.data)); grow > 0 {
		if f.used+grow > f.Limit {
			return 0, &fs.PathError{Op: "write", Path: name, Err: ErrFull}
		}
		f.used += grow
		n.data = append(n.data, make([]byte, grow)...)
	}
	copy(n.data[off:], p)
	n.modTime = time.Now()
	return len(p), nil
}

// Truncate sets a file's size.
func (f *FS) Truncate(name string, size int64) error {
	name, err := clean("truncate", name)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.walk(name)
	if err != nil {
		return &fs.PathError{Op: "truncate", Path: name, Err: err}
	}
	if n.dir {
		return &fs.PathError{Op: "truncate", Path: name, Err: errors.New("is a directory")}
	}
	grow := size - int64(len(n.data))
	if f.used+grow > f.Limit {
		return &fs.PathError{Op: "truncate", Path: name, Err: ErrFull}
	}
	f.used += grow
	if grow > 0 {
		n.data = append(n.data, make([]byte, grow)...)
	} else {
		n.data = n.data[:size:size]
	}
	n.modTime = time.Now()
	return nil
}

// Mkdir makes a directory.
func (f *FS) Mkdir(name string) error {
	name, err := clean("mkdir", name)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, base, err := f.parent("mkdir", name)
	if err != nil {
		return err
	}
	if d.kids[base] != nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	d.kids[base] = &node{name: base, dir: true, kids: map[string]*node{}, modTime: time.Now(), mode: fs.ModeDir | 0o755}
	return nil
}

// Remove removes a file or an empty directory.
func (f *FS) Remove(name string) error {
	name, err := clean("remove", name)
	if err != nil {
		return err
	}
	if name == "." {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrPermission}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, base, err := f.parent("remove", name)
	if err != nil {
		return err
	}
	n := d.kids[base]
	if n == nil {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	if n.dir && len(n.kids) > 0 {
		return &fs.PathError{Op: "remove", Path: name, Err: errors.New("directory not empty")}
	}
	f.used -= int64(len(n.data))
	delete(d.kids, base)
	return nil
}

// Rename moves a file or directory, replacing a file at the target.
func (f *FS) Rename(from, to string) error {
	from, err := clean("rename", from)
	if err != nil {
		return err
	}
	to, err = clean("rename", to)
	if err != nil {
		return err
	}
	if from == "." || to == "." || strings.HasPrefix(to+"/", from+"/") {
		return &fs.PathError{Op: "rename", Path: from, Err: fs.ErrInvalid}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fd, fb, err := f.parent("rename", from)
	if err != nil {
		return err
	}
	n := fd.kids[fb]
	if n == nil {
		return &fs.PathError{Op: "rename", Path: from, Err: fs.ErrNotExist}
	}
	td, tb, err := f.parent("rename", to)
	if err != nil {
		return err
	}
	if old := td.kids[tb]; old != nil {
		if old.dir {
			return &fs.PathError{Op: "rename", Path: to, Err: fs.ErrExist}
		}
		f.used -= int64(len(old.data))
	}
	delete(fd.kids, fb)
	n.name = tb
	td.kids[tb] = n
	return nil
}

// ReadAt reads from a file's current contents (unlike Open, which reads a
// snapshot taken when the file was opened).
func (f *FS) ReadAt(name string, p []byte, off int64) (int, error) {
	name, err := clean("read", name)
	if err != nil {
		return 0, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	n, err := f.walk(name)
	if err != nil {
		return 0, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	if n.dir {
		return 0, &fs.PathError{Op: "read", Path: name, Err: errors.New("is a directory")}
	}
	if off >= int64(len(n.data)) {
		return 0, io.EOF
	}
	c := copy(p, n.data[off:])
	if c < len(p) {
		return c, io.EOF
	}
	return c, nil
}
