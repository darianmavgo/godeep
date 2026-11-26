package block

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// wmem is a writable in-memory disk.
type wmem struct{ mem }

func (m *wmem) WriteBlocks(_ context.Context, lba uint64, p []byte) error {
	if err := Check(m, lba, p); err != nil {
		return err
	}
	copy(m.data[lba*uint64(m.bs):], p)
	return nil
}

// fat32 makes a fake FAT32 boot sector with label (enough for the policy).
func fat32(label string, size int) []byte {
	b := make([]byte, size)
	copy(b[0x47:0x52], label+"           ")
	copy(b[0x52:], "FAT32   ")
	b[510], b[511] = 0x55, 0xaa
	return b
}

func TestFence(t *testing.T) {
	ctx := context.Background()
	for _, bs := range []int{512, 4096} { // the Mac's SSD has 4 KB blocks
		img, err := MakeGPT(16<<20, bs, []PartSpec{
			{"Macintosh HD", TypeAPFS, bytes.Repeat([]byte{0xaa}, 1<<20)},
			{"", TypeBasicData, fat32("GODEEPDATA", 1<<20)},
			{"", TypeBasicData, fat32("OTHER", 1<<20)},
			{"", TypeLinux, fat32("GODEEPDATA", 1<<20)}, // right label, wrong type
		})
		if err != nil {
			t.Fatal(err)
		}
		disk := &wmem{mem{bs: bs, data: img}}
		f, err := Fence(ctx, disk, BasicDataFAT32("GODEEPDATA"))
		if err != nil {
			t.Fatal(err)
		}
		if got := f.Allowed(); len(got) != 1 || got[0] != "p2 " {
			t.Fatalf("allowed %q", got)
		}
		if Writable(f) {
			t.Fatal("the fenced disk as a whole is writable")
		}
		parts, _, err := Partitions(ctx, f)
		if err != nil || len(parts) != 4 {
			t.Fatalf("%d partitions, %v", len(parts), err)
		}
		blk := bytes.Repeat([]byte{0x5a}, bs)
		for i, p := range parts {
			want := i == 1
			if Writable(p) != want {
				t.Fatalf("bs %d: p%d writable %v", bs, p.Index, !want)
			}
			err := p.WriteBlocks(ctx, 1, blk)
			if want && err != nil || !want && !errors.Is(err, ErrFenced) {
				t.Fatalf("bs %d: p%d write: %v", bs, p.Index, err)
			}
		}
		if err := f.WriteBlocks(ctx, 1, blk); !errors.Is(err, ErrFenced) {
			t.Fatalf("write to the GPT: %v", err)
		}
		p2 := parts[1]
		two := append(append([]byte{}, blk...), blk...)
		if err := f.WriteBlocks(ctx, p2.Start+p2.Count-1, two); !errors.Is(err, ErrFenced) {
			t.Fatalf("write straddling the partition's end: %v", err)
		}
		// through the cache, as filesystems write
		r := NewReader(ctx, parts[1])
		if _, err := r.WriteAt([]byte("hello"), 8192); err != nil {
			t.Fatal(err)
		}
		if _, err := NewReader(ctx, parts[0]).WriteAt([]byte("hello"), 8192); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("cached write to APFS: %v", err)
		}
		if !bytes.Equal(img[p2.Start*uint64(bs)+8192:][:5], []byte("hello")) {
			t.Fatal("allowed write didn't land")
		}
		apfs := parts[0]
		if img[apfs.Start*uint64(bs)+uint64(bs)] != 0xaa {
			t.Fatal("APFS changed")
		}
	}
}
