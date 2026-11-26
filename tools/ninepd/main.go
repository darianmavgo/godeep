// ninepd serves a directory over 9P2000.L on TCP, for godeep to mount
// (mount9p HOST:PORT NAME): the dev machine's files on the Mac, or in QEMU
// through its NAT (the host is 10.0.2.2 from the guest).
//
//	go run ./tools/ninepd -addr 127.0.0.1:5640 -dir bin/share [-ro]
//
// Every path is resolved inside -dir with os.Root, so a client can't
// escape it (no "..", no symlinks out). It speaks what fs/ninep uses:
// version, attach, walk, getattr, lopen, lcreate, read, write, readdir,
// setattr (size), mkdir, unlinkat, renameat, clunk.
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path"
	"strings"
	"syscall"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:5640", "listen address")
	dir := flag.String("dir", ".", "directory to serve")
	ro := flag.Bool("ro", false, "read-only")
	flag.Parse()
	root, err := os.OpenRoot(*dir)
	if err != nil {
		log.Fatal(err)
	}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("ninepd: serving %s on %s (read-only: %v)", *dir, l.Addr(), *ro)
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			if err := Serve(c, root, *ro); err != nil && !errors.Is(err, io.EOF) {
				log.Printf("ninepd: %s: %v", c.RemoteAddr(), err)
			}
		}()
	}
}

const maxMsize = 1 << 20

// Serve answers one connection's requests in order.
func Serve(c io.ReadWriteCloser, root *os.Root, ro bool) error {
	defer c.Close()
	s := &server{root: root, ro: ro, fids: map[uint32]*fid{}, msize: maxMsize}
	defer s.closeAll()
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return err
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		if n < 7 || n > maxMsize {
			return fmt.Errorf("%d-byte message", n)
		}
		msg := make([]byte, n)
		copy(msg, hdr[:])
		if _, err := io.ReadFull(c, msg[4:]); err != nil {
			return err
		}
		if _, err := c.Write(s.handle(msg)); err != nil {
			return err
		}
	}
}

type fid struct {
	path string // relative to the root, "." for the root
	file *os.File
}

type server struct {
	root  *os.Root
	ro    bool
	fids  map[uint32]*fid
	msize int
}

func (s *server) closeAll() {
	for _, f := range s.fids {
		if f.file != nil {
			f.file.Close()
		}
	}
}

// ---- encoding ----

type rd struct {
	b   []byte
	bad bool
}

func (r *rd) take(n int) []byte {
	if len(r.b) < n {
		r.bad = true
		return make([]byte, n)
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}
func (r *rd) u8() byte    { return r.take(1)[0] }
func (r *rd) u16() uint16 { return binary.LittleEndian.Uint16(r.take(2)) }
func (r *rd) u32() uint32 { return binary.LittleEndian.Uint32(r.take(4)) }
func (r *rd) u64() uint64 { return binary.LittleEndian.Uint64(r.take(8)) }
func (r *rd) str() string { return string(r.take(int(r.u16()))) }

type wr struct{ b []byte }

func (w *wr) u8(v byte)    { w.b = append(w.b, v) }
func (w *wr) u16(v uint16) { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *wr) u32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *wr) u64(v uint64) { w.b = binary.LittleEndian.AppendUint64(w.b, v) }
func (w *wr) str(s string) { w.u16(uint16(len(s))); w.b = append(w.b, s...) }

func (w *wr) qid(fi fs.FileInfo) {
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

func frame(typ byte, tag uint16, body []byte) []byte {
	out := make([]byte, 7, 7+len(body))
	binary.LittleEndian.PutUint32(out, uint32(7+len(body)))
	out[4] = typ
	binary.LittleEndian.PutUint16(out[5:], tag)
	return append(out, body...)
}

func errno(err error) uint32 {
	var e syscall.Errno
	switch {
	case errors.As(err, &e):
		return uint32(e)
	case errors.Is(err, fs.ErrNotExist):
		return uint32(syscall.ENOENT)
	case errors.Is(err, fs.ErrExist):
		return uint32(syscall.EEXIST)
	case errors.Is(err, fs.ErrPermission):
		return uint32(syscall.EACCES)
	}
	return uint32(syscall.EIO)
}

// ---- requests ----

func (s *server) handle(msg []byte) []byte {
	typ, tag := msg[4], binary.LittleEndian.Uint16(msg[5:])
	r := &rd{b: msg[7:]}
	body, err := s.request(typ, r)
	if err == nil && r.bad {
		err = syscall.EINVAL
	}
	if err != nil {
		var w wr
		w.u32(errno(err))
		return frame(7, tag, w.b) // Rlerror
	}
	return frame(typ+1, tag, body)
}

func (s *server) get(r *rd) (*fid, error) {
	f := s.fids[r.u32()]
	if f == nil {
		return nil, syscall.EBADF
	}
	return f, nil
}

func (s *server) writable() error {
	if s.ro {
		return syscall.EROFS
	}
	return nil
}

// join adds a name to a path, refusing anything that isn't one component.
func join(dir, name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return "", syscall.EINVAL
	}
	return path.Join(dir, name), nil
}

func (s *server) request(typ byte, r *rd) ([]byte, error) {
	var w wr
	switch typ {
	case 100: // version
		msize := int(r.u32())
		s.msize = min(msize, maxMsize)
		w.u32(uint32(s.msize))
		w.str("9P2000.L")
	case 104: // attach
		n := r.u32()
		fi, err := s.root.Stat(".")
		if err != nil {
			return nil, err
		}
		s.fids[n] = &fid{path: "."}
		w.qid(fi)
	case 110: // walk
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
			next, err := join(cur, name)
			if err != nil {
				return nil, err
			}
			fi, err := s.root.Lstat(next)
			if err != nil {
				if len(qids) == 0 {
					return nil, err
				}
				break // a partial walk: qids so far, no new fid
			}
			qids = append(qids, fi)
			cur = next
		}
		w.u16(uint16(len(qids)))
		for _, fi := range qids {
			w.qid(fi)
		}
		if len(qids) == n { // the whole walk (n == 0: a clone)
			s.fids[newfid] = &fid{path: cur}
		}
	case 24: // getattr
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
		w.u64(0x7ff)
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
		for i := 0; i < 3; i++ { // atime, mtime, ctime
			w.u64(t)
			w.u64(ns)
		}
		for i := 0; i < 4; i++ { // btime, gen, data_version
			w.u64(0)
		}
	case 12: // lopen
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
			if mode != os.O_RDONLY || flags&0x200 != 0 {
				if err := s.writable(); err != nil {
					return nil, err
				}
			}
			if flags&0x200 != 0 {
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
	case 14: // lcreate
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		name, flags, perm := r.str(), r.u32(), r.u32()
		r.u32()
		if err := s.writable(); err != nil {
			return nil, err
		}
		p, err := join(f.path, name)
		if err != nil {
			return nil, err
		}
		mode := os.O_CREATE | os.O_RDWR
		if flags&0x80 != 0 {
			mode |= os.O_EXCL
		}
		if flags&0x200 != 0 {
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
	case 116: // read
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
	case 118: // write
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
	case 40: // readdir
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
		var data wr
		for i := int(off); i < len(ents); i++ {
			fi, err := ents[i].Info()
			if err != nil {
				continue
			}
			var e wr
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
	case 26: // setattr
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		valid := r.u32()
		r.u32()
		r.u32()
		r.u32()
		size := r.u64()
		if valid&0x8 != 0 {
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
	case 72: // mkdir
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		name, perm := r.str(), r.u32()
		r.u32()
		if err := s.writable(); err != nil {
			return nil, err
		}
		p, err := join(f.path, name)
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
	case 76: // unlinkat
		f, err := s.get(r)
		if err != nil {
			return nil, err
		}
		name := r.str()
		r.u32()
		if err := s.writable(); err != nil {
			return nil, err
		}
		p, err := join(f.path, name)
		if err != nil {
			return nil, err
		}
		if err := s.root.Remove(p); err != nil {
			return nil, err
		}
	case 74: // renameat
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
		op, err := join(of.path, oname)
		if err != nil {
			return nil, err
		}
		np, err := join(nf.path, nname)
		if err != nil {
			return nil, err
		}
		if err := s.root.Rename(op, np); err != nil {
			return nil, err
		}
	case 120: // clunk
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
