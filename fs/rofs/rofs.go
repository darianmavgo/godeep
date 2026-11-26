// Package rofs turns a read-only filesystem's nodes into an io/fs.FS: path
// walking (with symlinks), fs.File for files, fs.ReadDirFile for
// directories. A filesystem package (fs/fat, fs/ext4) only decodes its
// on-disk format into Nodes.
package rofs

import (
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
)

// Node is one file, directory or symlink.
type Node interface {
	Info() (fs.FileInfo, error) // Name() is the entry's name ("." for the root)
	ReadAt(p []byte, off int64) (int, error)
	ReadDir() ([]Node, error)  // directories
	Readlink() (string, error) // symlinks
}

// FS is an fs.FS over a tree of Nodes.
type FS struct {
	Root func() (Node, error)
	// Fold: names match case-insensitively (FAT).
	Fold bool
}

const maxLinks = 16

// Open implements fs.FS. Symlinks are followed, also in the last element
// (as os.Open does); targets that leave the filesystem are errors.
func (f FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	n, err := f.walk(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	info, err := n.Info()
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	if name != "." {
		info = renamed{info, path.Base(name)}
	}
	if info.IsDir() {
		return &dir{n: n, info: info}, nil
	}
	return &file{n: n, info: info}, nil
}

func (f FS) walk(name string) (Node, error) {
	root, err := f.Root()
	if err != nil {
		return nil, err
	}
	todo := split(name)
	var stack []Node // directories from the root to the current one
	cur := root
	links := 0
	for len(todo) > 0 {
		el := todo[0]
		todo = todo[1:]
		switch el {
		case ".":
			continue
		case "..":
			if len(stack) == 0 {
				return nil, errors.New("path leaves the filesystem")
			}
			cur, stack = stack[len(stack)-1], stack[:len(stack)-1]
			continue
		}
		next, err := f.child(cur, el)
		if err != nil {
			return nil, err
		}
		info, err := next.Info()
		if err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if links++; links > maxLinks {
				return nil, errors.New("too many levels of symbolic links")
			}
			target, err := next.Readlink()
			if err != nil {
				return nil, err
			}
			if strings.HasPrefix(target, "/") {
				cur, stack = root, nil
			}
			todo = append(split(target), todo...)
			continue
		}
		stack = append(stack, cur)
		cur = next
	}
	return cur, nil
}

func split(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (f FS) child(d Node, name string) (Node, error) {
	info, err := d.Info()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fs.ErrNotExist // a file used as a directory
	}
	kids, err := d.ReadDir()
	if err != nil {
		return nil, err
	}
	for _, k := range kids {
		ki, err := k.Info()
		if err != nil {
			continue
		}
		if ki.Name() == name || f.Fold && strings.EqualFold(ki.Name(), name) {
			return k, nil
		}
	}
	return nil, fs.ErrNotExist
}

// renamed reports the name it was opened by (FAT matches case-insensitively).
type renamed struct {
	fs.FileInfo
	name string
}

func (r renamed) Name() string { return r.name }

type file struct {
	n    Node
	info fs.FileInfo
	off  int64
}

func (f *file) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *file) Close() error               { return nil }

func (f *file) Read(p []byte) (int, error) {
	if f.off >= f.info.Size() {
		return 0, io.EOF
	}
	if rest := f.info.Size() - f.off; int64(len(p)) > rest {
		p = p[:rest]
	}
	n, err := f.n.ReadAt(p, f.off)
	f.off += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.info.Size() {
		return 0, io.EOF
	}
	want := len(p)
	if rest := f.info.Size() - off; int64(len(p)) > rest {
		p = p[:rest]
	}
	n, err := f.n.ReadAt(p, off)
	if err == nil && n < want {
		err = io.EOF
	}
	return n, err
}

func (f *file) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		off += f.off
	case io.SeekEnd:
		off += f.info.Size()
	}
	if off < 0 {
		return 0, errors.New("negative position")
	}
	f.off = off
	return off, nil
}

type dir struct {
	n      Node
	info   fs.FileInfo
	ents   []fs.DirEntry
	loaded bool
}

func (d *dir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *dir) Close() error               { return nil }
func (d *dir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.Name(), Err: errors.New("is a directory")}
}

func (d *dir) ReadDir(count int) ([]fs.DirEntry, error) {
	if !d.loaded {
		kids, err := d.n.ReadDir()
		if err != nil {
			return nil, err
		}
		for _, k := range kids {
			ki, err := k.Info()
			if err != nil {
				return nil, err
			}
			d.ents = append(d.ents, fs.FileInfoToDirEntry(ki))
		}
		d.loaded = true
	}
	if count <= 0 {
		out := d.ents
		d.ents = nil
		return out, nil
	}
	if len(d.ents) == 0 {
		return nil, io.EOF
	}
	n := min(count, len(d.ents))
	out := d.ents[:n]
	d.ents = d.ents[n:]
	return out, nil
}
