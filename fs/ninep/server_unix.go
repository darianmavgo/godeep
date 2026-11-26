//go:build unix && !tamago

package ninep

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
)

const serverMaxMsize = 1 << 20

// Serve answers one 9P2000.L connection's requests in order, rooted at root.
// Every path is confined inside root via os.Root (no ".." or symlink escapes).
// When ro is true, all mutating requests fail with EROFS.
func Serve(c io.ReadWriteCloser, root *os.Root, ro bool) error {
	defer c.Close()
	s := &srv{root: root, ro: ro, fids: map[uint32]*srvFid{}, msize: serverMaxMsize}
	defer s.closeAll()
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return err
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		if n < 7 || n > serverMaxMsize {
			return fmt.Errorf("%d-byte message", n)
		}
		buf := make([]byte, n)
		copy(buf, hdr[:])
		if _, err := io.ReadFull(c, buf[4:]); err != nil {
			return err
		}
		if _, err := c.Write(s.handle(buf)); err != nil {
			return err
		}
	}
}

type srvFid struct {
	path string // relative to root, "." for root
	file *os.File
}

type srv struct {
	root  *os.Root
	ro    bool
	fids  map[uint32]*srvFid
	msize int
}

func (s *srv) closeAll() {
	for _, f := range s.fids {
		if f.file != nil {
			f.file.Close()
		}
	}
}

type srvRd struct {
	b   []byte
	bad bool
}

func (r *srvRd) take(n int) []byte {
	if len(r.b) < n {
		r.bad = true
		return make([]byte, n)
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}
func (r *srvRd) u8() byte    { return r.take(1)[0] }
func (r *srvRd) u16() uint16 { return binary.LittleEndian.Uint16(r.take(2)) }
func (r *srvRd) u32() uint32 { return binary.LittleEndian.Uint32(r.take(4)) }
func (r *srvRd) u64() uint64 { return binary.LittleEndian.Uint64(r.take(8)) }
func (r *srvRd) str() string { return string(r.take(int(r.u16()))) }

type srvWr struct{ b []byte }

func (w *srvWr) u8(v byte)    { w.b = append(w.b, v) }
func (w *srvWr) u16(v uint16) { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *srvWr) u32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *srvWr) u64(v uint64) { w.b = binary.LittleEndian.AppendUint64(w.b, v) }
func (w *srvWr) str(s string) { w.u16(uint16(len(s))); w.b = append(w.b, s...) }

func (w *srvWr) qid(fi fs.FileInfo) {
	t := byte(0)
	if fi.IsDir() {
		t = 0x80
	}
	w.u8(t)
	w.u32(uint32(fi.ModTime().Unix()))
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	w.u64(ino)
}

func srvFrame(typ byte, tag uint16, body []byte) []byte {
	out := make([]byte, 7, 7+len(body))
	binary.LittleEndian.PutUint32(out, uint32(7+len(body)))
	out[4] = typ
	binary.LittleEndian.PutUint16(out[5:], tag)
	return append(out, body...)
}

// linuxErrno translates host OS errors to Linux 9P2000.L errno numbers
// (important on macOS where e.g. ENOTEMPTY is 66 instead of Linux's 39).
func linuxErrno(err error) uint32 {
	var e syscall.Errno
	if errors.As(err, &e) {
		switch e {
		case syscall.EPERM:
			return 1
		case syscall.ENOENT:
			return 2
		case syscall.EIO:
			return 5
		case syscall.EBADF:
			return 9
		case syscall.EACCES:
			return 13
		case syscall.EEXIST:
			return 17
		case syscall.ENOTDIR:
			return 20
		case syscall.EISDIR:
			return 21
		case syscall.EINVAL:
			return 22
		case syscall.EROFS:
			return 30
		case syscall.ENOTEMPTY:
			return 39
		case syscall.EOPNOTSUPP:
			return 95
		}
		return uint32(e)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 2
	case errors.Is(err, fs.ErrExist):
		return 17
	case errors.Is(err, fs.ErrPermission):
		return 13
	}
	return 5 // EIO
}

func (s *srv) handle(buf []byte) []byte {
	typ, tag := buf[4], binary.LittleEndian.Uint16(buf[5:])
	r := &srvRd{b: buf[7:]}
	body, err := s.request(typ, r)
	if err == nil && r.bad {
		err = syscall.EINVAL
	}
	if err != nil {
		var w srvWr
		w.u32(linuxErrno(err))
		return srvFrame(rlerror, tag, w.b)
	}
	return srvFrame(typ+1, tag, body)
}

func (s *srv) get(r *srvRd) (*srvFid, error) {
	f := s.fids[r.u32()]
	if f == nil {
		return nil, syscall.EBADF
	}
	return f, nil
}

func (s *srv) writable() error {
	if s.ro {
		return syscall.EROFS
	}
	return nil
}

func srvJoin(dir, name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return "", syscall.EINVAL
	}
	return path.Join(dir, name), nil
}

func (s *srv) request(typ byte, r *srvRd) ([]byte, error) {
	var w srvWr
	switch typ {
	case tversion:
		msize := int(r.u32())
		s.msize = min(msize, serverMaxMsize)
		w.u32(uint32(s.msize))
		w.str("9P2000.L")
	case tattach:
		n := r.u32()
		fi, err := s.root.Stat(".")
		if err != nil {
			return nil, err
		}
		s.fids[n] = &srvFid{path: "."}
		w.qid(fi)
	case twalk:
		from, err := s.get(r)
		if err != nil {
			return nil, err
		}
		newfid := r.u32()
		n := int(r.u16())
		names := make([]string, n)
		for i := range names {
			names[i] = r.str()
		}
		cur := from.path
		var qids []fs.FileInfo
		for _, name := range names {
			next, err := srvJoin(cur, name)
			if err != nil {
				return nil, err
			}
			fi, err := s.root.Lstat(next)
			if err != nil {
				if len(qids) == 0 {
					return nil, err
				}
				break
			}
			qids = append(qids, fi)
			cur = next
		}
		w.u16(uint16(len(qids)))
		for _, fi := range qids {
			w.qid(fi)
		}
		if len(qids) == n {
			s.fids[newfid] = &srvFid{path: cur}
		}
	case tgetattr:
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		r.u64()
		fi, err := s.root.Lstat(f.path)
		if err != nil {
			return nil, err
		}
		mode := uint32(fi.Mode().Perm())
		switch {
		case fi.IsDir():
			mode |= 0o040000
		case fi.Mode()&fs.ModeSymlink != 0:
			mode |= 0o120000
		default:
			mode |= 0o100000
		}
		w.u64(getattrBasic)
		w.qid(fi)
		w.u32(mode)
		w.u32(0)
		w.u32(0)
		w.u64(1)
		w.u64(0)
		w.u64(uint64(fi.Size()))
		w.u64(4096)
		w.u64(uint64(fi.Size()+511) / 512)
		t := uint64(fi.ModTime().Unix())
		ns := uint64(fi.ModTime().Nanosecond())
		for i := 0; i < 3; i++ {
			w.u64(t)
			w.u64(ns)
		}
		for i := 0; i < 4; i++ {
			w.u64(0)
		}
	case tlopen:
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		flags := r.u32()
		fi, err := s.root.Lstat(f.path)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			mode := os.O_RDONLY
			switch flags & 3 {
			case 1:
				mode = os.O_WRONLY
			case 2:
				mode = os.O_RDWR
			}
			if mode != os.O_RDONLY || flags&oTrunc != 0 {
				if err := s.writable(); err != nil {
					return nil, err
				}
			}
			if flags&oTrunc != 0 {
				mode |= os.O_TRUNC
			}
			file, err := s.root.OpenFile(f.path, mode, 0)
			if err != nil {
				return nil, err
			}
			f.file = file
		}
		w.qid(fi)
		w.u32(0)
	case tlcreate:
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		name, flags, perm := r.str(), r.u32(), r.u32()
		r.u32()
		if err := s.writable(); err != nil {
			return nil, err
		}
		p, err := srvJoin(f.path, name)
		if err != nil {
			return nil, err
		}
		mode := os.O_CREATE | os.O_RDWR
		if flags&0x80 != 0 {
			mode |= os.O_EXCL
		}
		if flags&oTrunc != 0 {
			mode |= os.O_TRUNC
		}
		file, err := s.root.OpenFile(p, mode, fs.FileMode(perm&0o777))
		if err != nil {
			return nil, err
		}
		fi, _ := file.Stat()
		f.path, f.file = p, file
		w.qid(fi)
		w.u32(0)
	case tread:
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		off, n := r.u64(), r.u32()
		if f.file == nil {
			return nil, syscall.EBADF
		}
		buf := make([]byte, min(int(n), s.msize-11))
		k, err := f.file.ReadAt(buf, int64(off))
		if err != nil && err != io.EOF {
			return nil, err
		}
		w.u32(uint32(k))
		w.b = append(w.b, buf[:k]...)
	case twrite:
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		off, n := r.u64(), r.u32()
		data := r.take(int(n))
		if f.file == nil {
			return nil, syscall.EBADF
		}
		k, err := f.file.WriteAt(data, int64(off))
		if err != nil {
			return nil, err
		}
		w.u32(uint32(k))
	case treaddir:
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		off, count := r.u64(), r.u32()
		d, err := s.root.Open(f.path)
		if err != nil {
			return nil, err
		}
		ents, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return nil, err
		}
		var data srvWr
		for i := int(off); i < len(ents); i++ {
			fi, err := ents[i].Info()
			if err != nil {
				continue
			}
			var e srvWr
			e.qid(fi)
			e.u64(uint64(i + 1))
			e.u8(0)
			e.str(ents[i].Name())
			if len(data.b)+len(e.b) > int(count) {
				break
			}
			data.b = append(data.b, e.b...)
		}
		w.u32(uint32(len(data.b)))
		w.b = append(w.b, data.b...)
	case tsetattr:
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		valid := r.u32()
		r.u32()
		r.u32()
		r.u32()
		size := r.u64()
		if valid&setSize != 0 {
			if err := s.writable(); err != nil {
				return nil, err
			}
			file, err := s.root.OpenFile(f.path, os.O_WRONLY, 0)
			if err != nil {
				return nil, err
			}
			err = file.Truncate(int64(size))
			file.Close()
			if err != nil {
				return nil, err
			}
		}
	case tmkdir:
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		name, perm := r.str(), r.u32()
		r.u32()
		if err := s.writable(); err != nil {
			return nil, err
		}
		p, err := srvJoin(f.path, name)
		if err != nil {
			return nil, err
		}
		if err := s.root.Mkdir(p, fs.FileMode(perm&0o777)); err != nil {
			return nil, err
		}
		fi, err := s.root.Lstat(p)
		if err != nil {
			return nil, err
		}
		w.qid(fi)
	case tunlinkat:
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		name := r.str()
		r.u32()
		if err := s.writable(); err != nil {
			return nil, err
		}
		p, err := srvJoin(f.path, name)
		if err != nil {
			return nil, err
		}
		if err := s.root.Remove(p); err != nil {
			return nil, err
		}
	case trenameat:
		of, err := s.get(r)
		if err != nil {
			return nil, err
		}
		oname := r.str()
		nf, err := s.get(r)
		if err != nil {
			return nil, err
		}
		nname := r.str()
		if err := s.writable(); err != nil {
			return nil, err
		}
		op, err := srvJoin(of.path, oname)
		if err != nil {
			return nil, err
		}
		np, err := srvJoin(nf.path, nname)
		if err != nil {
			return nil, err
		}
		if err := s.root.Rename(op, np); err != nil {
			return nil, err
		}
	case tclunk:
		n := r.u32()
		if f := s.fids[n]; f != nil && f.file != nil {
			f.file.Close()
		}
		delete(s.fids, n)
	default:
		return nil, syscall.EOPNOTSUPP
	}
	return w.b, nil
}
