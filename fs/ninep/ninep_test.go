package ninep

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// server is a 9P2000.L server over an fs.FS, enough for the client.
type server struct {
	fsys  fs.FS
	msize int
	mu    sync.Mutex
	fids  map[uint32]string
	rpcs  int
}

func (s *server) MaxMessage() int { return s.msize }

func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

func (s *server) RPC(_ context.Context, req []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rpcs++
	if int(le32(req)) != len(req) || len(req) > s.msize {
		return nil, errors.New("bad request size")
	}
	typ := req[4]
	r := &reply{b: req[7:]}
	var m msg
	fail := func(errno uint32) ([]byte, error) {
		var e msg
		e.u32(errno)
		return frame(rlerror, e), nil
	}
	switch typ {
	case tversion:
		m.u32(r.u32())
		m.str(r.str())
	case tattach:
		s.fids[r.u32()] = "."
		m.u8(0x80)
		m.u32(0)
		m.u64(1)
	case twalk:
		from, to := s.fids[r.u32()], r.u32()
		n := int(r.u16())
		cur := from
		walked := 0
		for i := 0; i < n; i++ {
			next := path.Join(cur, r.str())
			if _, err := fs.Stat(s.fsys, next); err != nil {
				break
			}
			cur, walked = next, walked+1
		}
		if n > 0 && walked == 0 {
			return fail(2)
		}
		if walked == n {
			s.fids[to] = cur
		}
		m.u16(uint16(walked))
		for i := 0; i < walked; i++ {
			m.u8(0)
			m.u32(0)
			m.u64(uint64(i))
		}
	case tgetattr:
		fi, err := fs.Stat(s.fsys, s.fids[r.u32()])
		if err != nil {
			return fail(2)
		}
		mode := uint32(fi.Mode().Perm()) | 0o100000
		if fi.IsDir() {
			mode = uint32(fi.Mode().Perm()) | 0o040000
		}
		m.u64(getattrBasic)
		m.u8(0)
		m.u32(0)
		m.u64(0)
		m.u32(mode)
		m.u32(0)
		m.u32(0)
		m.u64(1)
		m.u64(0)
		m.u64(uint64(fi.Size()))
		m.u64(4096)
		m.u64(0)
		m.u64(0)
		m.u64(0)
		m.u64(uint64(fi.ModTime().Unix()))
		m.u64(0)
		for i := 0; i < 6; i++ {
			m.u64(0)
		}
	case tlopen:
		m.u8(0)
		m.u32(0)
		m.u64(0)
		m.u32(0)
	case tread:
		name := s.fids[r.u32()]
		off, count := r.u64(), r.u32()
		f, ok := s.fsys.(fstest.MapFS)[name] // no copy per read
		if !ok {
			return fail(21)
		}
		b := f.Data
		if off > uint64(len(b)) {
			off = uint64(len(b))
		}
		data := b[off:min(uint64(len(b)), off+uint64(count))]
		m.u32(uint32(len(data)))
		m.b = append(m.b, data...)
	case treaddir:
		name := s.fids[r.u32()]
		off, count := r.u64(), r.u32()
		ents, err := fs.ReadDir(s.fsys, name)
		if err != nil {
			return fail(20)
		}
		all := append([]string{".", ".."}, func() []string {
			var n []string
			for _, e := range ents {
				n = append(n, e.Name())
			}
			return n
		}()...)
		var data msg
		for i := int(off); i < len(all); i++ {
			var e msg
			e.u8(0)
			e.u32(0)
			e.u64(uint64(i))
			e.u64(uint64(i + 1))
			e.u8(0)
			e.str(all[i])
			if len(data.b)+len(e.b) > int(count) {
				break
			}
			data.b = append(data.b, e.b...)
		}
		m.u32(uint32(len(data.b)))
		m.b = append(m.b, data.b...)
	case tclunk:
		delete(s.fids, r.u32())
	default:
		return fail(95)
	}
	return frame(typ+1, m), nil
}

func frame(typ byte, m msg) []byte {
	out := make([]byte, 7, 7+len(m.b))
	binary.LittleEndian.PutUint32(out, uint32(7+len(m.b)))
	out[4] = typ
	binary.LittleEndian.PutUint16(out[5:], 1)
	return append(out, m.b...)
}

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"hello.txt":        {Data: []byte("hello from the host\n"), ModTime: time.Unix(1700000000, 0)},
		"bin/wasm/x.wasm":  {Data: []byte(strings.Repeat("\x00asm", 50000))}, // 200 KB: many reads
		"docs/a/b/deep.md": {Data: []byte("deep")},
	}
}

func TestClient(t *testing.T) {
	srv := &server{fsys: testFS(), msize: 8192, fids: map[uint32]string{}}
	c, err := Mount(context.Background(), srv, "host")
	if err != nil {
		t.Fatal(err)
	}
	if err := fstest.TestFS(c, "hello.txt", "bin/wasm/x.wasm", "docs/a/b/deep.md"); err != nil {
		t.Fatal(err)
	}
	b, err := fs.ReadFile(c, "bin/wasm/x.wasm")
	if err != nil || len(b) != 200000 {
		t.Fatalf("big file: %d bytes, %v", len(b), err)
	}
	if fi, err := fs.Stat(c, "hello.txt"); err != nil || fi.ModTime().Unix() != 1700000000 || fi.Size() != 20 {
		t.Fatalf("stat: %v %v", fi, err)
	}
	if _, err := c.Open("nope/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	// Fids are given back: no leak across many opens.
	srv.mu.Lock()
	open := len(srv.fids)
	srv.mu.Unlock()
	if open != 1 {
		t.Fatalf("%d fids still open (want just the root)", open)
	}
	if c.Type() != "9p" || c.Label() != "host" {
		t.Fatal(c.Type(), c.Label())
	}
	var _ io.ReaderAt = (*file)(nil)
}
