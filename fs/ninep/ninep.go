// Package ninep is a 9P2000.L client: a directory on the host, shared by
// QEMU (-fsdev local + virtio-9p), read as an io/fs.FS. godeep mounts it as
// a volume named by the share's tag ("host:/bin/wasm/hello.wasm"), so files
// built on the host are visible in the guest at once: no copy, no reboot.
//
// Reads: version, attach, walk, getattr, lopen, read, readdir, clunk.
// Writes (write.go): lcreate, write, setattr (truncate), mkdir, unlinkat,
// renameat; whether the share accepts them is probed at mount. Requests go
// through a Transport, one at a time: virtio (drivers/virtio/p9) or TCP
// (DialTCP, for a server like tools/ninepd on the other computer).
package ninep

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Transport carries one request and returns its reply (both whole
// messages, size field included).
type Transport interface {
	RPC(ctx context.Context, req []byte) ([]byte, error)
	// MaxMessage is the largest message the transport carries (msize).
	MaxMessage() int
}

// Message types.
const (
	tlerror      = 7
	tlopen       = 12
	tgetattr     = 24
	treaddir     = 40
	tversion     = 100
	tattach      = 104
	twalk        = 110
	tread        = 116
	tclunk       = 120
	rlerror      = tlerror
	noFid        = ^uint32(0)
	getattrBasic = 0x000007ff
)

// Client is a mounted 9P filesystem. Safe for concurrent use.
type Client struct {
	t     Transport
	ctx   context.Context
	msize int
	tag   string

	mu       sync.Mutex
	nextFid  uint32
	freeFid  []uint32
	writable bool
}

// Errno is a Linux error number from the server (Rlerror).
type Errno uint32

func (e Errno) Error() string {
	switch e {
	case 2:
		return "no such file or directory"
	case 13:
		return "permission denied"
	case 20:
		return "not a directory"
	case 21:
		return "is a directory"
	case 17:
		return "file exists"
	case 30:
		return "read-only file system"
	case 39:
		return "directory not empty"
	}
	return fmt.Sprintf("9p error %d", uint32(e))
}

func (e Errno) Is(target error) bool {
	switch target {
	case fs.ErrNotExist:
		return e == 2
	case fs.ErrPermission:
		return e == 1 || e == 13 || e == 30
	case fs.ErrExist:
		return e == 17
	}
	return false
}

const rootFid = 0

// Mount negotiates the protocol and attaches to the share's root. ctx bounds
// every later request too (the driver's lifetime).
func Mount(ctx context.Context, t Transport, tag string) (*Client, error) {
	c := &Client{t: t, ctx: ctx, msize: t.MaxMessage(), tag: tag, nextFid: 1}
	var m msg
	m.u32(uint32(c.msize))
	m.str("9P2000.L")
	r, err := c.rpc(tversion, m)
	if err != nil {
		return nil, err
	}
	msize := int(r.u32())
	if v := r.str(); v != "9P2000.L" {
		return nil, fmt.Errorf("9p: server speaks %q, not 9P2000.L", v)
	}
	c.msize = min(c.msize, msize)
	m = msg{}
	m.u32(rootFid)
	m.u32(noFid)
	m.str("root")
	m.str("")
	m.u32(0) // n_uname
	if _, err := c.rpc(tattach, m); err != nil {
		return nil, fmt.Errorf("9p attach: %w", err)
	}
	c.writable = c.probeWrite()
	return c, nil
}

func (c *Client) Type() string  { return "9p" }
func (c *Client) Label() string { return c.tag }

// ---- messages ----

type msg struct{ b []byte }

func (m *msg) u8(v byte)    { m.b = append(m.b, v) }
func (m *msg) u16(v uint16) { m.b = binary.LittleEndian.AppendUint16(m.b, v) }
func (m *msg) u32(v uint32) { m.b = binary.LittleEndian.AppendUint32(m.b, v) }
func (m *msg) u64(v uint64) { m.b = binary.LittleEndian.AppendUint64(m.b, v) }
func (m *msg) str(s string) { m.u16(uint16(len(s))); m.b = append(m.b, s...) }

type reply struct {
	b   []byte
	err error
}

func (r *reply) take(n int) []byte {
	if len(r.b) < n {
		r.err = errors.New("9p: short reply")
		r.b = nil
		return make([]byte, n)
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}
func (r *reply) u8() byte    { return r.take(1)[0] }
func (r *reply) u16() uint16 { return binary.LittleEndian.Uint16(r.take(2)) }
func (r *reply) u32() uint32 { return binary.LittleEndian.Uint32(r.take(4)) }
func (r *reply) u64() uint64 { return binary.LittleEndian.Uint64(r.take(8)) }
func (r *reply) str() string { return string(r.take(int(r.u16()))) }

type qid struct {
	typ  byte
	vers uint32
	path uint64
}

func (r *reply) qid() qid { return qid{r.u8(), r.u32(), r.u64()} }

// rpc sends one request (type typ, body m) and returns the reply's body.
func (c *Client) rpc(typ byte, m msg) (*reply, error) {
	req := make([]byte, 7, 7+len(m.b))
	binary.LittleEndian.PutUint32(req, uint32(7+len(m.b)))
	req[4] = typ
	binary.LittleEndian.PutUint16(req[5:], 1) // one request at a time: one tag
	req = append(req, m.b...)
	resp, err := c.t.RPC(c.ctx, req)
	if err != nil {
		return nil, err
	}
	if len(resp) < 7 {
		return nil, errors.New("9p: short reply")
	}
	r := &reply{b: resp[7:]}
	switch resp[4] {
	case typ + 1:
		return r, nil
	case rlerror:
		return nil, Errno(r.u32())
	}
	return nil, fmt.Errorf("9p: reply type %d to request %d", resp[4], typ)
}

func (c *Client) fid() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.freeFid); n > 0 {
		f := c.freeFid[n-1]
		c.freeFid = c.freeFid[:n-1]
		return f
	}
	c.nextFid++
	return c.nextFid
}

func (c *Client) clunk(fid uint32) {
	var m msg
	m.u32(fid)
	if _, err := c.rpc(tclunk, m); err == nil {
		c.mu.Lock()
		c.freeFid = append(c.freeFid, fid)
		c.mu.Unlock()
	}
}

// walk returns a new fid for name (an io/fs path).
func (c *Client) walk(name string) (uint32, error) {
	var parts []string
	if name != "." {
		parts = strings.Split(name, "/")
	}
	fid := c.fid()
	var m msg
	m.u32(rootFid)
	m.u32(fid)
	m.u16(uint16(len(parts)))
	for _, p := range parts {
		m.str(p)
	}
	r, err := c.rpc(twalk, m)
	if err != nil {
		c.mu.Lock()
		c.freeFid = append(c.freeFid, fid)
		c.mu.Unlock()
		return 0, err
	}
	if n := int(r.u16()); n != len(parts) {
		c.clunk(fid)
		return 0, Errno(2) // a partial walk: the rest doesn't exist
	}
	return fid, nil
}

type stat struct {
	name    string
	mode    uint32
	size    int64
	modTime time.Time
}

func (s stat) Name() string { return s.name }
func (s stat) Size() int64  { return s.size }
func (s stat) Mode() fs.FileMode {
	m := fs.FileMode(s.mode & 0o777)
	switch s.mode & 0o170000 {
	case 0o040000:
		m |= fs.ModeDir
	case 0o120000:
		m |= fs.ModeSymlink
	}
	return m
}
func (s stat) ModTime() time.Time         { return s.modTime }
func (s stat) IsDir() bool                { return s.Mode().IsDir() }
func (s stat) Sys() any                   { return nil }
func (s stat) Type() fs.FileMode          { return s.Mode().Type() }
func (s stat) Info() (fs.FileInfo, error) { return s, nil }

func (c *Client) getattr(fid uint32, name string) (stat, error) {
	var m msg
	m.u32(fid)
	m.u64(getattrBasic)
	r, err := c.rpc(tgetattr, m)
	if err != nil {
		return stat{}, err
	}
	r.u64() // valid
	r.qid()
	st := stat{name: name, mode: r.u32()}
	r.u32() // uid
	r.u32() // gid
	r.u64() // nlink
	r.u64() // rdev
	st.size = int64(r.u64())
	r.u64() // blksize
	r.u64() // blocks
	r.u64() // atime
	r.u64()
	sec, nsec := r.u64(), r.u64()
	st.modTime = time.Unix(int64(sec), int64(nsec))
	return st, r.err
}

// ---- io/fs ----

func base(name string) string {
	if name == "." {
		return "."
	}
	return path.Base(name)
}

func (c *Client) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	fid, err := c.walk(name)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	defer c.clunk(fid)
	st, err := c.getattr(fid, base(name))
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return st, nil
}

func (c *Client) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	fid, err := c.walk(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	st, err := c.getattr(fid, base(name))
	if err == nil {
		var m msg
		m.u32(fid)
		m.u32(0) // O_RDONLY
		_, err = c.rpc(tlopen, m)
	}
	if err != nil {
		c.clunk(fid)
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	f := &file{c: c, fid: fid, st: st, name: name}
	if st.IsDir() {
		if err := f.loadDir(); err != nil {
			f.Close()
			return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
		}
	}
	return f, nil
}

func (c *Client) ReadDir(name string) ([]fs.DirEntry, error) {
	f, err := c.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d, ok := f.(fs.ReadDirFile)
	if !ok || !f.(*file).st.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	return d.ReadDir(-1)
}

type file struct {
	c      *Client
	fid    uint32
	st     stat
	name   string
	off    int64
	ents   []fs.DirEntry
	closed bool
}

func (f *file) Stat() (fs.FileInfo, error) { return f.st, nil }

func (f *file) Close() error {
	if !f.closed {
		f.closed = true
		f.c.clunk(f.fid)
	}
	return nil
}

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	if f.st.IsDir() {
		return 0, &fs.PathError{Op: "read", Path: f.name, Err: errors.New("is a directory")}
	}
	total := 0
	for total < len(p) {
		want := min(len(p)-total, f.c.msize-11) // Rread header: size, type, tag, count
		var m msg
		m.u32(f.fid)
		m.u64(uint64(off) + uint64(total))
		m.u32(uint32(want))
		r, err := f.c.rpc(tread, m)
		if err != nil {
			return total, err
		}
		n := int(r.u32())
		data := r.take(n)
		if r.err != nil {
			return total, r.err
		}
		copy(p[total:], data)
		total += n
		if n == 0 {
			return total, io.EOF
		}
	}
	return total, nil
}

func (f *file) Read(p []byte) (int, error) {
	n, err := f.ReadAt(p, f.off)
	f.off += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (f *file) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		off += f.off
	case io.SeekEnd:
		off += f.st.size
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	f.off = off
	return off, nil
}

// loadDir reads every entry (with its attributes: one walk+getattr each).
func (f *file) loadDir() error {
	var names []string
	var off uint64
	for {
		var m msg
		m.u32(f.fid)
		m.u64(off)
		m.u32(uint32(f.c.msize - 11))
		r, err := f.c.rpc(treaddir, m)
		if err != nil {
			return err
		}
		n := int(r.u32())
		data := &reply{b: r.take(n)}
		if n == 0 {
			break
		}
		for len(data.b) > 0 && data.err == nil {
			data.qid()
			off = data.u64()
			data.u8() // type
			name := data.str()
			if name != "." && name != ".." {
				names = append(names, name)
			}
		}
		if data.err != nil {
			return data.err
		}
	}
	sort.Strings(names)
	for _, n := range names {
		p := n
		if f.name != "." {
			p = f.name + "/" + n
		}
		st, err := f.c.Stat(p)
		if err != nil {
			continue // raced with a removal on the host
		}
		f.ents = append(f.ents, st.(stat))
	}
	return nil
}

func (f *file) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		out := f.ents
		f.ents = nil
		return out, nil
	}
	if len(f.ents) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(f.ents))
	out := f.ents[:n]
	f.ents = f.ents[n:]
	return out, nil
}
