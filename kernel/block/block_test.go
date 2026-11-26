package block

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

// mem is a disk in memory that counts reads.
type mem struct {
	bs    int
	data  []byte
	mu    sync.Mutex
	reads int
}

func (m *mem) BlockSize() int { return m.bs }
func (m *mem) Blocks() uint64 { return uint64(len(m.data) / m.bs) }
func (m *mem) ReadBlocks(_ context.Context, lba uint64, p []byte) error {
	if err := Check(m, lba, p); err != nil {
		return err
	}
	m.mu.Lock()
	m.reads++
	m.mu.Unlock()
	copy(p, m.data[lba*uint64(m.bs):])
	return nil
}

func TestGPT(t *testing.T) {
	a, b := bytes.Repeat([]byte("A"), 3000), bytes.Repeat([]byte("B"), 2<<20)
	img, err := MakeGPT(8<<20, 512, []PartSpec{{"GODEEPFAT", TypeBasicData, a}, {"godeep-ext4", TypeLinux, b}})
	if err != nil {
		t.Fatal(err)
	}
	d := &mem{bs: 512, data: img}
	parts, scheme, err := Partitions(context.Background(), d)
	if err != nil || scheme != "gpt" || len(parts) != 2 {
		t.Fatalf("%v %s %v", parts, scheme, err)
	}
	p1, p2 := parts[0], parts[1]
	if p1.Name != "GODEEPFAT" || p1.Type != "basic data" || p1.Start != 2048 || p1.Count != 6 {
		t.Fatalf("p1 %+v", p1)
	}
	if p2.Name != "godeep-ext4" || p2.Type != "Linux data" || p2.Start != 4096 || p2.Count != 4096 {
		t.Fatalf("p2 %+v", p2)
	}
	buf := make([]byte, 512)
	if err := p2.ReadBlocks(context.Background(), 4095, buf); err != nil || buf[0] != 'B' {
		t.Fatalf("read last block of p2: %v", err)
	}
	if err := p2.ReadBlocks(context.Background(), 4096, buf); err != ErrRange {
		t.Fatalf("read past p2: %v", err)
	}

	img[512+20] ^= 1 // corrupt the header
	if _, _, err := Partitions(context.Background(), d); err == nil {
		t.Fatal("corrupt GPT accepted")
	}
}

func TestMBR(t *testing.T) {
	img := make([]byte, 4<<20)
	e := img[446:]
	e[4] = 0x83
	e[8] = 63      // start
	e[12+1] = 0x10 // count 4096
	img[510], img[511] = 0x55, 0xaa
	parts, scheme, err := Partitions(context.Background(), &mem{bs: 512, data: img})
	if err != nil || scheme != "mbr" || len(parts) != 1 || parts[0].Start != 63 || parts[0].Count != 4096 || parts[0].Type != "MBR 0x83" {
		t.Fatalf("%+v %s %v", parts, scheme, err)
	}
	if _, _, err := Partitions(context.Background(), &mem{bs: 512, data: make([]byte, 4096)}); err != ErrNoTable {
		t.Fatalf("blank disk: %v", err)
	}
}

func TestReader(t *testing.T) {
	data := make([]byte, 300_000+512-300_000%512)
	for i := range data {
		data[i] = byte(i * 13)
	}
	d := &mem{bs: 512, data: data}
	r := NewReader(context.Background(), d)
	for _, c := range []struct{ off, n int }{{0, 10}, {65530, 20}, {100_000, 150_000}, {len(data) - 5, 5}} {
		p := make([]byte, c.n)
		if n, err := r.ReadAt(p, int64(c.off)); err != nil || n != c.n || !bytes.Equal(p, data[c.off:c.off+c.n]) {
			t.Fatalf("ReadAt(%d, %d): n=%d err=%v", c.off, c.n, n, err)
		}
	}
	reads := d.reads
	r.ReadAt(make([]byte, 100), 10) // cached
	if d.reads != reads {
		t.Fatal("cached chunk read again")
	}
	if n, err := r.ReadAt(make([]byte, 10), int64(len(data)-4)); n != 4 || err == nil {
		t.Fatalf("short read at the end: %d %v", n, err)
	}

	// concurrent readers
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := make([]byte, 1000)
			off := int64(i * 30_000)
			if _, err := r.ReadAt(p, off); err != nil || !bytes.Equal(p, data[off:off+1000]) {
				t.Errorf("concurrent read %d", i)
			}
		}(i)
	}
	wg.Wait()
}

// memDisk is a writable in-memory device.
type memDisk struct {
	bs   int
	data []byte
}

func (m *memDisk) BlockSize() int { return m.bs }
func (m *memDisk) Blocks() uint64 { return uint64(len(m.data) / m.bs) }
func (m *memDisk) ReadBlocks(_ context.Context, lba uint64, p []byte) error {
	if err := Check(m, lba, p); err != nil {
		return err
	}
	copy(p, m.data[lba*uint64(m.bs):])
	return nil
}
func (m *memDisk) WriteBlocks(_ context.Context, lba uint64, p []byte) error {
	if err := Check(m, lba, p); err != nil {
		return err
	}
	copy(m.data[lba*uint64(m.bs):], p)
	return nil
}

func TestWriteThroughCache(t *testing.T) {
	d := &memDisk{bs: 512, data: make([]byte, 1<<20)}
	r := NewReader(context.Background(), d)
	buf := make([]byte, 10)
	r.ReadAt(buf, 70000) // cache the chunk
	msg := []byte("spans a chunk boundary and several blocks")
	off := int64(ChunkSize - 7)
	if n, err := r.WriteAt(msg, off); err != nil || n != len(msg) {
		t.Fatalf("WriteAt: %d %v", n, err)
	}
	got := make([]byte, len(msg))
	if _, err := r.ReadAt(got, off); err != nil || string(got) != string(msg) {
		t.Fatalf("read back through the cache: %q %v", got, err)
	}
	if string(d.data[off:off+int64(len(msg))]) != string(msg) {
		t.Fatal("not written to the disk")
	}
	// a partition writes at its offset
	p := &Partition{Start: 100, Count: 100, parent: d}
	if !Writable(p) {
		t.Fatal("partition of a writable disk not writable")
	}
	pr := NewReader(context.Background(), p)
	pr.WriteAt([]byte("part"), 3)
	if string(d.data[100*512+3:100*512+7]) != "part" {
		t.Fatal("partition write at the wrong place")
	}
	ro := NewReader(context.Background(), readOnly{d})
	if _, err := ro.WriteAt([]byte("x"), 0); err != ErrReadOnly {
		t.Fatalf("read-only: %v", err)
	}
}

type readOnly struct{ Device }
