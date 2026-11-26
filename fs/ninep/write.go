package ninep

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path"
	"strings"
	"sync"
)

// More 9P2000.L requests (writing).
const (
	tlcreate  = 14
	tsetattr  = 26
	twrite    = 118
	tmkdir    = 72
	trenameat = 74
	tunlinkat = 76

	oWronly    = 0x1
	oRdwr      = 0x2
	oCreat     = 0x40
	oTrunc     = 0x200
	atRemovDir = 0x200
	setSize    = 0x8
	probeName  = ".godeep-write-probe"
)

// Writable reports whether the share accepts writes (probed at mount).
func (c *Client) Writable() bool { return c.writable }

// probeWrite creates and removes a file in the share's root: a read-only
// share refuses (EROFS) without creating anything.
func (c *Client) probeWrite() bool {
	fid, err := c.walk(".")
	if err != nil {
		return false
	}
	var m msg
	m.u32(fid)
	m.str(probeName)
	m.u32(oWronly | oCreat)
	m.u32(0o644)
	m.u32(0)
	if _, err := c.rpc(tlcreate, m); err != nil {
		c.clunk(fid)
		return false
	}
	c.clunk(fid)
	c.unlink(".", probeName, false)
	return true
}

func (c *Client) unlink(dir, name string, isDir bool) error {
	dfid, err := c.walk(dir)
	if err != nil {
		return err
	}
	defer c.clunk(dfid)
	var m msg
	m.u32(dfid)
	m.str(name)
	flags := uint32(0)
	if isDir {
		flags = atRemovDir
	}
	m.u32(flags)
	_, err = c.rpc(tunlinkat, m)
	return err
}

func split(name string) (dir, base string) {
	dir, base = path.Split(name)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "."
	}
	return dir, base
}

func (c *Client) check(op, name string) error {
	if !c.writable {
		return &fs.PathError{Op: op, Path: name, Err: Errno(30)}
	}
	if !fs.ValidPath(name) || name == "." {
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	return nil
}

// openWrite opens name for writing (creating it, truncating with trunc) and
// returns its fid.
func (c *Client) openWrite(name string, create, trunc bool) (uint32, error) {
	fid, err := c.walk(name)
	if err == nil {
		flags := uint32(oWronly)
		if trunc {
			flags |= oTrunc
		}
		var m msg
		m.u32(fid)
		m.u32(flags)
		if _, err := c.rpc(tlopen, m); err != nil {
			c.clunk(fid)
			return 0, err
		}
		return fid, nil
	}
	if !create || !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	dir, base := split(name)
	fid, err = c.walk(dir)
	if err != nil {
		return 0, err
	}
	var m msg
	m.u32(fid) // becomes the new, open file
	m.str(base)
	m.u32(oWronly | oCreat | oTrunc)
	m.u32(0o644)
	m.u32(0)
	if _, err := c.rpc(tlcreate, m); err != nil {
		c.clunk(fid)
		return 0, err
	}
	return fid, nil
}

func (c *Client) write(fid uint32, p []byte, off int64) (int, error) {
	done := 0
	for done < len(p) {
		n := min(len(p)-done, c.msize-23) // Twrite header: size, type, tag, fid, offset, count
		var m msg
		m.u32(fid)
		m.u64(uint64(off) + uint64(done))
		m.u32(uint32(n))
		m.b = append(m.b, p[done:done+n]...)
		r, err := c.rpc(twrite, m)
		if err != nil {
			return done, err
		}
		w := int(r.u32())
		if w == 0 {
			return done, io.ErrShortWrite
		}
		done += w
	}
	return done, nil
}

// WriteFile creates or replaces a file.
func (c *Client) WriteFile(name string, data []byte) error {
	if err := c.check("write", name); err != nil {
		return err
	}
	fid, err := c.openWrite(name, true, true)
	if err != nil {
		return &fs.PathError{Op: "write", Path: name, Err: err}
	}
	defer c.clunk(fid)
	if _, err := c.write(fid, data, 0); err != nil {
		return &fs.PathError{Op: "write", Path: name, Err: err}
	}
	return nil
}

// Create returns a writer for a new (or emptied) file.
func (c *Client) Create(name string) (io.WriteCloser, error) {
	if err := c.check("create", name); err != nil {
		return nil, err
	}
	fid, err := c.openWrite(name, true, true)
	if err != nil {
		return nil, &fs.PathError{Op: "create", Path: name, Err: err}
	}
	return &fileWriter{c: c, fid: fid}, nil
}

type fileWriter struct {
	c    *Client
	fid  uint32
	off  int64
	once sync.Once
}

func (w *fileWriter) Write(p []byte) (int, error) {
	n, err := w.c.write(w.fid, p, w.off)
	w.off += int64(n)
	return n, err
}

func (w *fileWriter) Close() error { w.once.Do(func() { w.c.clunk(w.fid) }); return nil }

// WriteAt writes into an existing file.
func (c *Client) WriteAt(name string, p []byte, off int64) (int, error) {
	if err := c.check("write", name); err != nil {
		return 0, err
	}
	fid, err := c.openWrite(name, false, false)
	if err != nil {
		return 0, &fs.PathError{Op: "write", Path: name, Err: err}
	}
	defer c.clunk(fid)
	return c.write(fid, p, off)
}

// ReadAt reads a file's current contents.
func (c *Client) ReadAt(name string, p []byte, off int64) (int, error) {
	f, err := c.Open(name)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.(*file).ReadAt(p, off)
}

// Truncate sets a file's size.
func (c *Client) Truncate(name string, size int64) error {
	if err := c.check("truncate", name); err != nil {
		return err
	}
	fid, err := c.walk(name)
	if err != nil {
		return &fs.PathError{Op: "truncate", Path: name, Err: err}
	}
	defer c.clunk(fid)
	var m msg
	m.u32(fid)
	m.u32(setSize)
	m.u32(0) // mode
	m.u32(0) // uid
	m.u32(0) // gid
	m.u64(uint64(size))
	for i := 0; i < 4; i++ { // atime, mtime (sec, nsec)
		m.u64(0)
	}
	if _, err := c.rpc(tsetattr, m); err != nil {
		return &fs.PathError{Op: "truncate", Path: name, Err: err}
	}
	return nil
}

// Mkdir makes a directory.
func (c *Client) Mkdir(name string) error {
	if err := c.check("mkdir", name); err != nil {
		return err
	}
	dir, base := split(name)
	dfid, err := c.walk(dir)
	if err != nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: err}
	}
	defer c.clunk(dfid)
	var m msg
	m.u32(dfid)
	m.str(base)
	m.u32(0o755)
	m.u32(0)
	if _, err := c.rpc(tmkdir, m); err != nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: err}
	}
	return nil
}

// Remove removes a file or an empty directory.
func (c *Client) Remove(name string) error {
	if err := c.check("remove", name); err != nil {
		return err
	}
	st, err := c.Stat(name)
	if err != nil {
		return err
	}
	dir, base := split(name)
	if err := c.unlink(dir, base, st.IsDir()); err != nil {
		return &fs.PathError{Op: "remove", Path: name, Err: err}
	}
	return nil
}

// Rename moves a file or directory.
func (c *Client) Rename(from, to string) error {
	if err := c.check("rename", from); err != nil {
		return err
	}
	if err := c.check("rename", to); err != nil {
		return err
	}
	od, ob := split(from)
	nd, nb := split(to)
	ofid, err := c.walk(od)
	if err != nil {
		return &fs.PathError{Op: "rename", Path: from, Err: err}
	}
	defer c.clunk(ofid)
	nfid, err := c.walk(nd)
	if err != nil {
		return &fs.PathError{Op: "rename", Path: to, Err: err}
	}
	defer c.clunk(nfid)
	var m msg
	m.u32(ofid)
	m.str(ob)
	m.u32(nfid)
	m.str(nb)
	if _, err := c.rpc(trenameat, m); err != nil {
		return &fs.PathError{Op: "rename", Path: from, Err: err}
	}
	return nil
}

// ---- TCP ----

// TCPTransport carries 9P over a stream (9P's own framing: each message
// starts with its size).
type TCPTransport struct {
	mu    sync.Mutex
	conn  net.Conn
	msize int
}

// DialTCP connects to a 9P server through dial (a cap.Net's Dial).
func DialTCP(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), addr string, msize int) (*TCPTransport, error) {
	c, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return &TCPTransport{conn: c, msize: msize}, nil
}

func (t *TCPTransport) MaxMessage() int { return t.msize }

func (t *TCPTransport) RPC(ctx context.Context, req []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { t.conn.Close() })
	defer stop()
	if _, err := t.conn.Write(req); err != nil {
		return nil, err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(t.conn, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n < 7 || int(n) > t.msize {
		return nil, fmt.Errorf("9p: %d-byte reply", n)
	}
	resp := make([]byte, n)
	copy(resp, hdr[:])
	if _, err := io.ReadFull(t.conn, resp[4:]); err != nil {
		return nil, err
	}
	return resp, nil
}

// Close closes the connection.
func (t *TCPTransport) Close() error { return t.conn.Close() }
