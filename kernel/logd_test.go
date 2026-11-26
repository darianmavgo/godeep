package kernel

import (
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"

	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/klog"
	"godeep/kernel/proc"
	"godeep/kernel/vfs"
)

// memDisk is a writable disk in memory.
type memDisk struct {
	mu   sync.Mutex
	bs   int
	data []byte
}

func (m *memDisk) BlockSize() int { return m.bs }
func (m *memDisk) Blocks() uint64 { return uint64(len(m.data) / m.bs) }
func (m *memDisk) ReadBlocks(_ context.Context, lba uint64, p []byte) error {
	if err := block.Check(m, lba, p); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	copy(p, m.data[lba*uint64(m.bs):])
	return nil
}
func (m *memDisk) WriteBlocks(_ context.Context, lba uint64, p []byte) error {
	if err := block.Check(m, lba, p); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	copy(m.data[lba*uint64(m.bs):], p)
	return nil
}

func fatImage(t *testing.T, label string) []byte {
	f, err := os.Open("../fs/testdata/fat32.img.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	copy(b[0x47:0x52], label+"           ")
	return b
}

func TestLogSaver(t *testing.T) {
	img, err := block.MakeGPT(128<<20, 512, []block.PartSpec{
		{Type: block.TypeBasicData, Data: fatImage(t, "OTHER")},
		{Type: block.TypeBasicData, Data: fatImage(t, "GODEEPDATA")},
	})
	if err != nil {
		t.Fatal(err)
	}
	disk := &memDisk{bs: 512, data: img}
	dev.Register(dev.Driver{Compatible: "test,logdisk", Run: func(ctx context.Context, d *dev.Device) error {
		d.Ready(disk)
		<-ctx.Done()
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := dev.NewManager(proc.NewTable(), nil, nil)
	mgr.Start(ctx, []dev.Node{{Name: "disk0", Compatible: "test,logdisk"}})
	if _, err := mgr.Get(ctx, "disk0"); err != nil {
		t.Fatal(err)
	}
	ns := vfs.New(ctx, mgr)
	log := klog.New(io.Discard, 0)
	s := &logSaver{label: "GODEEPDATA", mgr: mgr, fs: ns, log: log}

	read := func(name string) string {
		t.Helper()
		v, err := ns.Open(ctx, "disk0p2")
		if err != nil {
			t.Fatal(err)
		}
		b, err := fs.ReadFile(v, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return string(b)
	}
	log.Printf("first boot line")
	if err := s.save(ctx); err != nil {
		t.Fatal(err)
	}
	if s.vol != "disk0p2" {
		t.Fatalf("saved to %q, want the GODEEPDATA partition", s.vol)
	}
	if got := read(logFile); !strings.Contains(got, "first boot line") || !strings.Contains(got, "logd: saving") {
		t.Fatalf("log.txt: %q", got)
	}
	log.Printf("more")
	s.flush()
	if got := read(logFile); !strings.Contains(got, "more") {
		t.Fatalf("not updated: %q", got)
	}

	// the next boot: the old log becomes log.prev.txt
	log2 := klog.New(io.Discard, 0)
	log2.Printf("second boot")
	s2 := &logSaver{label: "GODEEPDATA", mgr: mgr, fs: ns, log: log2}
	if err := s2.save(ctx); err != nil {
		t.Fatal(err)
	}
	if prev, cur := read(logPrev), read(logFile); !strings.Contains(prev, "first boot line") || !strings.Contains(cur, "second boot") || strings.Contains(cur, "first boot") {
		t.Fatalf("rotation: prev %q cur %q", prev, cur)
	}
	// the other FAT32 partition is untouched
	v, _ := ns.Open(ctx, "disk0p1")
	if _, err := fs.Stat(v, logFile); err == nil {
		t.Fatal("log written to the wrong partition")
	}
}
