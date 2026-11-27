// Package synth builds read-only io/fs filesystems whose contents are
// generated when they're opened: proc: and sys: (fs/procfs). A file's bytes
// are produced once per Open, so a reader sees one consistent snapshot.
package synth

import (
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// Node is a file (Data set) or a directory (List and Get set).
type Node struct {
	Data func() []byte           // a file's contents, generated on open
	List func() []string         // a directory's entry names
	Get  func(name string) *Node // a directory's entry, nil if gone
}

// File returns a file node.
func File(data func() []byte) *Node { return &Node{Data: data} }

// Text returns a file node from a string generator.
func Text(s func() string) *Node { return &Node{Data: func() []byte { return []byte(s()) }} }

// Static returns a directory with a fixed set of entries.
func Static(entries map[string]*Node) *Node {
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return &Node{
		List: func() []string { return names },
		Get:  func(name string) *Node { return entries[name] },
	}
}

func (n *Node) isDir() bool { return n.Data == nil }

// FS is a synthetic filesystem; it is a vfs.Volume.
//
// Like Linux's /proc, every file reports size 0 (its contents exist only
// once it's opened) and every entry the filesystem's creation time.
type FS struct {
	root       *Node
	typ, label string
	mod        time.Time
}

// New returns a filesystem over root.
func New(typ, label string, root *Node) *FS {
	return &FS{root: root, typ: typ, label: label, mod: time.Now()}
}

func (f *FS) Type() string  { return f.typ }
func (f *FS) Label() string { return f.label }

func (f *FS) lookup(op, name string) (*Node, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	n := f.root
	if name == "." {
		return n, nil
	}
	for _, el := range strings.Split(name, "/") {
		if !n.isDir() {
			return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
		}
		if n = n.Get(el); n == nil {
			return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
		}
	}
	return n, nil
}

// Open implements fs.FS.
func (f *FS) Open(name string) (fs.File, error) {
	n, err := f.lookup("open", name)
	if err != nil {
		return nil, err
	}
	if !n.isDir() {
		return &file{info: info{name: path.Base(name), mod: f.mod}, data: n.Data()}, nil
	}
	return &dir{info: info{name: path.Base(name), dir: true, mod: f.mod}, entries: entries(n, f.mod)}, nil
}

// ReadDir implements fs.ReadDirFS.
func (f *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	n, err := f.lookup("readdir", name)
	if err != nil {
		return nil, err
	}
	if !n.isDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	return entries(n, f.mod), nil
}

// Stat implements fs.StatFS.
func (f *FS) Stat(name string) (fs.FileInfo, error) {
	n, err := f.lookup("stat", name)
	if err != nil {
		return nil, err
	}
	return info{name: path.Base(name), dir: n.isDir(), mod: f.mod}, nil
}

func entries(n *Node, now time.Time) []fs.DirEntry {
	names := n.List()
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	out := make([]fs.DirEntry, 0, len(sorted))
	for _, name := range sorted {
		c := n.Get(name)
		if c == nil {
			continue // went away between List and Get
		}
		out = append(out, fs.FileInfoToDirEntry(info{name: name, dir: c.isDir(), mod: now}))
	}
	return out
}

type info struct {
	name string
	size int64
	dir  bool
	mod  time.Time
}

func (i info) Name() string       { return i.name }
func (i info) Size() int64        { return i.size }
func (i info) ModTime() time.Time { return i.mod }
func (i info) IsDir() bool        { return i.dir }
func (i info) Sys() any           { return nil }
func (i info) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

type file struct {
	info info
	data []byte
	off  int64
}

func (f *file) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *file) Close() error               { return nil }
func (f *file) Read(b []byte) (int, error) {
	if f.off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(b, f.data[f.off:])
	f.off += int64(n)
	return n, nil
}

func (f *file) ReadAt(b []byte, off int64) (int, error) {
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(b, f.data[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

func (f *file) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		off += f.off
	case io.SeekEnd:
		off += int64(len(f.data))
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	f.off = off
	return off, nil
}

type dir struct {
	info    info
	entries []fs.DirEntry
	pos     int
}

func (d *dir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *dir) Close() error               { return nil }
func (d *dir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: errors.New("is a directory")}
}

func (d *dir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.entries[d.pos:]
	if n <= 0 {
		d.pos = len(d.entries)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if n > len(rest) {
		n = len(rest)
	}
	d.pos += n
	return rest[:n], nil
}
