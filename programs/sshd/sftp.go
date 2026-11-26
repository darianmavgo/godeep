package sshd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"godeep/kernel/vfs"
)

// SFTP version 3 (draft-ietf-secsh-filexfer-02), the version OpenSSH
// speaks, over a session's "sftp" subsystem. It's what scp uses by default
// (OpenSSH 9+), so both `scp` and `sftp` work against godeep.
//
// The tree is the shell's namespace: "/" lists the volumes as
// directories, "/ram/x.wasm" is ram:/x.wasm, "/nvme0p2/docs" is
// nvme0p2:/docs. Relative paths start at /ram (the writable volume), so
// `scp file godeep:` lands in RAM. Only writable volumes (ram) accept
// writes; disks are read-only.

// Packet types.
const (
	fxpInit     = 1
	fxpVersion  = 2
	fxpOpen     = 3
	fxpClose    = 4
	fxpRead     = 5
	fxpWrite    = 6
	fxpLstat    = 7
	fxpFstat    = 8
	fxpSetstat  = 9
	fxpFsetstat = 10
	fxpOpendir  = 11
	fxpReaddir  = 12
	fxpRemove   = 13
	fxpMkdir    = 14
	fxpRmdir    = 15
	fxpRealpath = 16
	fxpStat     = 17
	fxpRename   = 18
	fxpReadlink = 19
	fxpStatus   = 101
	fxpHandle   = 102
	fxpData     = 103
	fxpName     = 104
	fxpAttrs    = 105
)

// Status codes.
const (
	fxOK               = 0
	fxEOF              = 1
	fxNoSuchFile       = 2
	fxPermissionDenied = 3
	fxFailure          = 4
	fxBadMessage       = 5
	fxOpUnsupported    = 8
)

// Open flags and attribute flags.
const (
	fxfRead   = 0x01
	fxfWrite  = 0x02
	fxfAppend = 0x04
	fxfCreat  = 0x08
	fxfTrunc  = 0x10
	fxfExcl   = 0x20

	attrSize        = 0x01
	attrUIDGID      = 0x02
	attrPermissions = 0x04
	attrACModTime   = 0x08
	attrExtended    = 0x80000000
)

const (
	maxPacket = 256 << 10
	maxRead   = 64 << 10
	sftpHome  = "/ram"
	sIFDIR    = 0o040000
	sIFREG    = 0o100000
)

type sftpServer struct {
	ns      *vfs.NS
	ctx     context.Context
	handles map[string]*handle
	next    int
}

type handle struct {
	vol   vfs.Volume
	path  string
	data  []byte        // a file opened for reading: its contents
	write bool          // opened for writing (vol is writable)
	dir   []fs.FileInfo // an open directory's remaining entries
	isDir bool
}

// ServeSFTP serves one SFTP session over rw until EOF or ctx ends.
func ServeSFTP(ctx context.Context, rw io.ReadWriter, ns *vfs.NS) error {
	s := &sftpServer{ns: ns, ctx: ctx, handles: map[string]*handle{}}
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(rw, hdr[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 || n > maxPacket {
			return fmt.Errorf("sftp: packet of %d bytes", n)
		}
		pkt := make([]byte, n)
		if _, err := io.ReadFull(rw, pkt); err != nil {
			return err
		}
		reply := s.handle(pkt)
		if reply == nil {
			continue
		}
		out := make([]byte, 4, 4+len(reply))
		binary.BigEndian.PutUint32(out, uint32(len(reply)))
		if _, err := rw.Write(append(out, reply...)); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// ---- wire encoding ----

type reader struct {
	b   []byte
	err error
}

func (r *reader) u8() byte {
	if len(r.b) < 1 {
		r.err = errBad
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *reader) u32() uint32 {
	if len(r.b) < 4 {
		r.err = errBad
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) u64() uint64 {
	if len(r.b) < 8 {
		r.err = errBad
		return 0
	}
	v := binary.BigEndian.Uint64(r.b)
	r.b = r.b[8:]
	return v
}

func (r *reader) str() string {
	n := r.u32()
	if r.err != nil || uint32(len(r.b)) < n {
		r.err = errBad
		return ""
	}
	v := string(r.b[:n])
	r.b = r.b[n:]
	return v
}

// attrs parses ATTRS, returning the size if given (-1 if not).
func (r *reader) attrs() int64 {
	f := r.u32()
	size := int64(-1)
	if f&attrSize != 0 {
		size = int64(r.u64())
	}
	if f&attrUIDGID != 0 {
		r.u32()
		r.u32()
	}
	if f&attrPermissions != 0 {
		r.u32()
	}
	if f&attrACModTime != 0 {
		r.u32()
		r.u32()
	}
	if f&attrExtended != 0 {
		for n := r.u32(); n > 0 && r.err == nil; n-- {
			r.str()
			r.str()
		}
	}
	return size
}

var errBad = errors.New("malformed packet")

type writer struct{ bytes.Buffer }

func (w *writer) u8(v byte)    { w.WriteByte(v) }
func (w *writer) u32(v uint32) { w.Write(binary.BigEndian.AppendUint32(nil, v)) }
func (w *writer) u64(v uint64) { w.Write(binary.BigEndian.AppendUint64(nil, v)) }
func (w *writer) str(s string) { w.u32(uint32(len(s))); w.WriteString(s) }

func (w *writer) attrs(fi fs.FileInfo) {
	w.u32(attrSize | attrPermissions | attrACModTime)
	w.u64(uint64(fi.Size()))
	w.u32(mode(fi))
	t := uint32(fi.ModTime().Unix())
	w.u32(t)
	w.u32(t)
}

func mode(fi fs.FileInfo) uint32 {
	m := uint32(fi.Mode().Perm())
	if fi.IsDir() {
		return m | sIFDIR
	}
	return m | sIFREG
}

func status(id uint32, code uint32, msg string) []byte {
	var w writer
	w.u8(fxpStatus)
	w.u32(id)
	w.u32(code)
	w.str(msg)
	w.str("")
	return w.Bytes()
}

func errStatus(id uint32, err error) []byte {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return status(id, fxNoSuchFile, err.Error())
	case errors.Is(err, fs.ErrPermission), errors.Is(err, vfs.ErrReadOnly):
		return status(id, fxPermissionDenied, err.Error())
	}
	return status(id, fxFailure, err.Error())
}

// ---- paths ----

// abs makes p absolute (relative to the home, /ram) and clean.
func abs(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = sftpHome + "/" + p
	}
	return path.Clean(p)
}

// resolve splits an absolute path into its volume and io/fs path. The root
// "/" has no volume.
func (s *sftpServer) resolve(p string) (vfs.Volume, string, error) {
	p = abs(p)
	if p == "/" {
		return nil, ".", nil
	}
	vol, rest, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	v, err := s.ns.Open(s.ctx, vol)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", fs.ErrNotExist, err)
	}
	return v, vfs.Clean(rest), nil
}

// rootInfo describes "/" and the volumes in it.
type dirInfo struct{ name string }

func (d dirInfo) Name() string       { return d.name }
func (d dirInfo) Size() int64        { return 0 }
func (d dirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (d dirInfo) ModTime() time.Time { return time.Time{} }
func (d dirInfo) IsDir() bool        { return true }
func (d dirInfo) Sys() any           { return nil }

func (s *sftpServer) stat(p string) (fs.FileInfo, error) {
	v, rel, err := s.resolve(p)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return dirInfo{"/"}, nil
	}
	if rel == "." {
		return dirInfo{path.Base(abs(p))}, nil
	}
	return fs.Stat(v, rel)
}

func writableVol(v vfs.Volume) (vfs.Writable, error) {
	if vfs.IsWritable(v) {
		return v.(vfs.Writable), nil
	}
	return nil, vfs.ErrReadOnly
}

// ---- requests ----

func (s *sftpServer) newHandle(h *handle) []byte {
	s.next++
	name := strconv.Itoa(s.next)
	s.handles[name] = h
	return []byte(name)
}

func (s *sftpServer) handle(pkt []byte) []byte {
	r := &reader{b: pkt}
	typ := r.u8()
	if typ == fxpInit {
		var w writer
		w.u8(fxpVersion)
		w.u32(3)
		return w.Bytes()
	}
	id := r.u32()
	if r.err != nil {
		return status(id, fxBadMessage, "short packet")
	}
	reply := s.request(typ, id, r)
	if r.err != nil {
		return status(id, fxBadMessage, r.err.Error())
	}
	return reply
}

func (s *sftpServer) request(typ byte, id uint32, r *reader) []byte {
	switch typ {
	case fxpRealpath:
		p := abs(r.str())
		var w writer
		w.u8(fxpName)
		w.u32(id)
		w.u32(1)
		w.str(p)
		w.str(p)
		w.attrs(dirInfo{path.Base(p)})
		return w.Bytes()

	case fxpStat, fxpLstat:
		fi, err := s.stat(r.str())
		if err != nil {
			return errStatus(id, err)
		}
		return attrsReply(id, fi)

	case fxpFstat:
		h := s.handles[r.str()]
		if h == nil {
			return status(id, fxFailure, "bad handle")
		}
		if h.isDir {
			return attrsReply(id, dirInfo{path.Base(h.path)})
		}
		fi, err := fs.Stat(h.vol, h.path)
		if err != nil {
			return errStatus(id, err)
		}
		return attrsReply(id, fi)

	case fxpOpen:
		name := r.str()
		flags := r.u32()
		r.attrs()
		if r.err != nil {
			return nil
		}
		v, rel, err := s.resolve(name)
		if err != nil {
			return errStatus(id, err)
		}
		if v == nil || rel == "." {
			return status(id, fxFailure, "is a directory")
		}
		if flags&(fxfWrite|fxfAppend|fxfCreat|fxfTrunc) == 0 {
			data, err := fs.ReadFile(v, rel)
			if err != nil {
				return errStatus(id, err)
			}
			return handleReply(id, s.newHandle(&handle{vol: v, path: rel, data: data}))
		}
		w, err := writableVol(v)
		if err != nil {
			return errStatus(id, err)
		}
		_, statErr := fs.Stat(w, rel)
		exists := statErr == nil
		switch {
		case exists && flags&fxfExcl != 0:
			return status(id, fxFailure, "file exists")
		case !exists && flags&fxfCreat == 0:
			return status(id, fxNoSuchFile, "no such file")
		case !exists || flags&fxfTrunc != 0:
			if err := w.WriteFile(rel, nil); err != nil {
				return errStatus(id, err)
			}
		}
		return handleReply(id, s.newHandle(&handle{vol: v, path: rel, write: true}))

	case fxpClose:
		hn := r.str()
		if s.handles[hn] == nil {
			return status(id, fxFailure, "bad handle")
		}
		delete(s.handles, hn)
		return status(id, fxOK, "")

	case fxpRead:
		h := s.handles[r.str()]
		off := r.u64()
		n := r.u32()
		if r.err != nil {
			return nil
		}
		if h == nil || h.isDir {
			return status(id, fxFailure, "bad handle")
		}
		data := h.data
		if h.write { // reading a file open for writing: its current contents
			var err error
			if data, err = fs.ReadFile(h.vol, h.path); err != nil {
				return errStatus(id, err)
			}
		}
		if off >= uint64(len(data)) {
			return status(id, fxEOF, "")
		}
		end := min(uint64(len(data)), off+uint64(min(n, maxRead)))
		var w writer
		w.u8(fxpData)
		w.u32(id)
		w.str(string(data[off:end]))
		return w.Bytes()

	case fxpWrite:
		h := s.handles[r.str()]
		off := r.u64()
		data := r.str()
		if r.err != nil {
			return nil
		}
		if h == nil || !h.write {
			return status(id, fxFailure, "not open for writing")
		}
		if _, err := h.vol.(vfs.Writable).WriteAt(h.path, []byte(data), int64(off)); err != nil {
			return errStatus(id, err)
		}
		return status(id, fxOK, "")

	case fxpSetstat, fxpFsetstat:
		var v vfs.Volume
		var rel string
		if typ == fxpSetstat {
			var err error
			if v, rel, err = s.resolve(r.str()); err != nil {
				return errStatus(id, err)
			}
		} else {
			h := s.handles[r.str()]
			if h == nil {
				return status(id, fxFailure, "bad handle")
			}
			v, rel = h.vol, h.path
		}
		size := r.attrs()
		if r.err != nil {
			return nil
		}
		if size >= 0 && v != nil {
			w, err := writableVol(v)
			if err != nil {
				return errStatus(id, err)
			}
			if err := w.Truncate(rel, size); err != nil {
				return errStatus(id, err)
			}
		}
		return status(id, fxOK, "") // modes and times: accepted, not kept

	case fxpOpendir:
		p := r.str()
		v, rel, err := s.resolve(p)
		if err != nil {
			return errStatus(id, err)
		}
		var infos []fs.FileInfo
		if v == nil {
			for _, name := range s.ns.Names(s.ctx) {
				infos = append(infos, dirInfo{name})
			}
		} else {
			ents, err := fs.ReadDir(v, rel)
			if err != nil {
				return errStatus(id, err)
			}
			for _, e := range ents {
				if fi, err := e.Info(); err == nil {
					infos = append(infos, fi)
				}
			}
		}
		return handleReply(id, s.newHandle(&handle{vol: v, path: rel, dir: infos, isDir: true}))

	case fxpReaddir:
		h := s.handles[r.str()]
		if h == nil || !h.isDir {
			return status(id, fxFailure, "bad handle")
		}
		if len(h.dir) == 0 {
			return status(id, fxEOF, "")
		}
		batch := h.dir[:min(len(h.dir), 100)]
		h.dir = h.dir[len(batch):]
		var w writer
		w.u8(fxpName)
		w.u32(id)
		w.u32(uint32(len(batch)))
		for _, fi := range batch {
			w.str(fi.Name())
			w.str(longname(fi))
			w.attrs(fi)
		}
		return w.Bytes()

	case fxpRemove, fxpRmdir, fxpMkdir:
		v, rel, err := s.resolve(r.str())
		if typ == fxpMkdir {
			r.attrs()
		}
		if err != nil {
			return errStatus(id, err)
		}
		if v == nil || rel == "." {
			return status(id, fxPermissionDenied, "volumes can't be changed")
		}
		w, err := writableVol(v)
		if err != nil {
			return errStatus(id, err)
		}
		if typ == fxpMkdir {
			err = w.Mkdir(rel)
		} else {
			err = w.Remove(rel)
		}
		if err != nil {
			return errStatus(id, err)
		}
		return status(id, fxOK, "")

	case fxpRename:
		fv, from, err := s.resolve(r.str())
		if err != nil {
			return errStatus(id, err)
		}
		tv, to, err := s.resolve(r.str())
		if err != nil {
			return errStatus(id, err)
		}
		if fv == nil || tv == nil || fv != tv {
			return status(id, fxFailure, "rename within one volume only")
		}
		w, err := writableVol(fv)
		if err != nil {
			return errStatus(id, err)
		}
		if err := w.Rename(from, to); err != nil {
			return errStatus(id, err)
		}
		return status(id, fxOK, "")
	}
	return status(id, fxOpUnsupported, "unsupported request")
}

func attrsReply(id uint32, fi fs.FileInfo) []byte {
	var w writer
	w.u8(fxpAttrs)
	w.u32(id)
	w.attrs(fi)
	return w.Bytes()
}

func handleReply(id uint32, h []byte) []byte {
	var w writer
	w.u8(fxpHandle)
	w.u32(id)
	w.str(string(h))
	return w.Bytes()
}

// longname is the `ls -l` line sftp's ls prints.
func longname(fi fs.FileInfo) string {
	return fmt.Sprintf("%s 1 root root %8d %s %s", fi.Mode(), fi.Size(), fi.ModTime().UTC().Format("Jan _2 15:04"), fi.Name())
}
