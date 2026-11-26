package wasm

import (
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"

	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/tetratelabs/wazero/sys"

	"godeep/fs/memfs"
	"godeep/kernel/vfs"
)

// writableFS lets a WASI program change a writable volume (ram:): wazero's
// experimental sys.FS over vfs.Writable. Read-only volumes are mounted as
// plain fs.FS instead.
type writableFS struct {
	experimentalsys.UnimplementedFS
	w vfs.Writable
}

func clean(p string) string {
	p = strings.Trim(path.Clean("/"+p), "/")
	if p == "" {
		return "."
	}
	return p
}

func errno(err error) experimentalsys.Errno {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, fs.ErrNotExist):
		return experimentalsys.ENOENT
	case errors.Is(err, fs.ErrExist):
		return experimentalsys.EEXIST
	case errors.Is(err, fs.ErrInvalid):
		return experimentalsys.EINVAL
	case errors.Is(err, fs.ErrPermission), errors.Is(err, vfs.ErrReadOnly):
		return experimentalsys.EPERM
	case errors.Is(err, memfs.ErrFull):
		return experimentalsys.EIO // WASI has ENOSPC, wazero's sys doesn't
	}
	switch msg := err.Error(); {
	case strings.Contains(msg, "not empty"):
		return experimentalsys.ENOTEMPTY
	case strings.Contains(msg, "is a directory"):
		return experimentalsys.EISDIR
	case strings.Contains(msg, "not a directory"):
		return experimentalsys.ENOTDIR
	}
	return experimentalsys.EIO
}

func (f *writableFS) Stat(p string) (sys.Stat_t, experimentalsys.Errno) {
	fi, err := fs.Stat(f.w, clean(p))
	if err != nil {
		return sys.Stat_t{}, errno(err)
	}
	return sys.NewStat_t(fi), 0
}

func (f *writableFS) Lstat(p string) (sys.Stat_t, experimentalsys.Errno) { return f.Stat(p) }

func (f *writableFS) OpenFile(p string, flag experimentalsys.Oflag, _ fs.FileMode) (experimentalsys.File, experimentalsys.Errno) {
	name := clean(p)
	fi, err := fs.Stat(f.w, name)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errno(err)
	}
	if exists && fi.IsDir() {
		if flag&(experimentalsys.O_WRONLY|experimentalsys.O_RDWR) != 0 {
			return nil, experimentalsys.EISDIR
		}
		return &file{fs: f, name: name, dir: true}, 0
	}
	if flag&experimentalsys.O_DIRECTORY != 0 {
		if !exists {
			return nil, experimentalsys.ENOENT
		}
		return nil, experimentalsys.ENOTDIR
	}
	switch {
	case exists && flag&experimentalsys.O_CREAT != 0 && flag&experimentalsys.O_EXCL != 0:
		return nil, experimentalsys.EEXIST
	case !exists && flag&experimentalsys.O_CREAT == 0:
		return nil, experimentalsys.ENOENT
	case !exists || flag&experimentalsys.O_TRUNC != 0:
		if err := f.w.WriteFile(name, nil); err != nil {
			return nil, errno(err)
		}
	}
	return &file{fs: f, name: name, append: flag&experimentalsys.O_APPEND != 0,
		write: flag&(experimentalsys.O_WRONLY|experimentalsys.O_RDWR) != 0}, 0
}

func (f *writableFS) Mkdir(p string, _ fs.FileMode) experimentalsys.Errno {
	return errno(f.w.Mkdir(clean(p)))
}

func (f *writableFS) Rename(from, to string) experimentalsys.Errno {
	return errno(f.w.Rename(clean(from), clean(to)))
}

func (f *writableFS) Rmdir(p string) experimentalsys.Errno {
	if fi, err := fs.Stat(f.w, clean(p)); err == nil && !fi.IsDir() {
		return experimentalsys.ENOTDIR
	}
	return errno(f.w.Remove(clean(p)))
}

func (f *writableFS) Unlink(p string) experimentalsys.Errno {
	if fi, err := fs.Stat(f.w, clean(p)); err == nil && fi.IsDir() {
		return experimentalsys.EISDIR
	}
	return errno(f.w.Remove(clean(p)))
}

func (f *writableFS) Chmod(string, fs.FileMode) experimentalsys.Errno    { return 0 } // modes aren't kept
func (f *writableFS) Utimens(string, int64, int64) experimentalsys.Errno { return 0 }

// file is an open file or directory.
type file struct {
	experimentalsys.UnimplementedFile
	fs     *writableFS
	name   string
	dir    bool
	write  bool
	append bool
	off    int64
	ents   []fs.DirEntry
	read   bool // directory entries loaded
}

func (h *file) IsDir() (bool, experimentalsys.Errno) { return h.dir, 0 }
func (h *file) IsAppend() bool                       { return h.append }
func (h *file) SetAppend(on bool) experimentalsys.Errno {
	h.append = on
	return 0
}

func (h *file) Stat() (sys.Stat_t, experimentalsys.Errno) { return h.fs.Stat(h.name) }

func (h *file) size() (int64, experimentalsys.Errno) {
	fi, err := fs.Stat(h.fs.w, h.name)
	if err != nil {
		return 0, errno(err)
	}
	return fi.Size(), 0
}

func (h *file) Pread(buf []byte, off int64) (int, experimentalsys.Errno) {
	if h.dir {
		return 0, experimentalsys.EISDIR
	}
	n, err := h.fs.w.ReadAt(h.name, buf, off)
	if err == io.EOF {
		err = nil
	}
	return n, errno(err)
}

func (h *file) Read(buf []byte) (int, experimentalsys.Errno) {
	n, e := h.Pread(buf, h.off)
	h.off += int64(n)
	return n, e
}

func (h *file) Seek(off int64, whence int) (int64, experimentalsys.Errno) {
	if h.dir {
		if off == 0 && whence == io.SeekStart {
			h.read, h.ents = false, nil // rewind
			return 0, 0
		}
		return 0, experimentalsys.EISDIR
	}
	base := int64(0)
	switch whence {
	case io.SeekCurrent:
		base = h.off
	case io.SeekEnd:
		size, e := h.size()
		if e != 0 {
			return 0, e
		}
		base = size
	}
	if base+off < 0 {
		return 0, experimentalsys.EINVAL
	}
	h.off = base + off
	return h.off, 0
}

func (h *file) Pwrite(buf []byte, off int64) (int, experimentalsys.Errno) {
	if !h.write {
		return 0, experimentalsys.EBADF
	}
	n, err := h.fs.w.WriteAt(h.name, buf, off)
	return n, errno(err)
}

func (h *file) Write(buf []byte) (int, experimentalsys.Errno) {
	if h.append {
		size, e := h.size()
		if e != 0 {
			return 0, e
		}
		h.off = size
	}
	n, e := h.Pwrite(buf, h.off)
	h.off += int64(n)
	return n, e
}

func (h *file) Truncate(size int64) experimentalsys.Errno {
	if !h.write {
		return experimentalsys.EBADF
	}
	return errno(h.fs.w.Truncate(h.name, size))
}

func (h *file) Readdir(n int) ([]experimentalsys.Dirent, experimentalsys.Errno) {
	if !h.dir {
		return nil, experimentalsys.ENOTDIR
	}
	if !h.read {
		ents, err := fs.ReadDir(h.fs.w, h.name)
		if err != nil {
			return nil, errno(err)
		}
		h.ents, h.read = ents, true
	}
	if n < 0 || n > len(h.ents) {
		n = len(h.ents)
	}
	out := make([]experimentalsys.Dirent, 0, n)
	for _, e := range h.ents[:n] {
		out = append(out, experimentalsys.Dirent{Name: e.Name(), Type: e.Type()})
	}
	h.ents = h.ents[n:]
	return out, 0
}

func (h *file) Sync() experimentalsys.Errno     { return 0 }
func (h *file) Datasync() experimentalsys.Errno { return 0 }
func (h *file) Utimens(int64, int64) experimentalsys.Errno {
	return 0
}
func (h *file) Close() experimentalsys.Errno { return 0 }
