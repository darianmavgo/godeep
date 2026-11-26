package sh

import (
	"bytes"
	"compress/gzip"
	"context"
	"godeep/fs/memfs"
	"godeep/kernel/vfs"
	"io"
	"os"
	"strings"
	"testing"

	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/proc"
	"godeep/kernel/tty"
)

func gunzip(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open("../../fs/testdata/" + name)
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
	return b
}

type memDisk []byte

func (m memDisk) BlockSize() int { return 512 }
func (m memDisk) Blocks() uint64 { return uint64(len(m) / 512) }
func (m memDisk) ReadBlocks(_ context.Context, lba uint64, p []byte) error {
	if err := block.Check(m, lba, p); err != nil {
		return err
	}
	copy(p, m[lba*512:])
	return nil
}

func TestDiskCommands(t *testing.T) {
	img, err := block.MakeGPT(64<<20, 512, []block.PartSpec{
		{Name: "GODEEPFAT", Type: block.TypeBasicData, Data: gunzip(t, "fat32.img.gz")},
		{Name: "godeep-ext4", Type: block.TypeLinux, Data: gunzip(t, "ext4.img.gz")},
	})
	if err != nil {
		t.Fatal(err)
	}
	dev.Register(dev.Driver{Compatible: "test,disk", Run: func(ctx context.Context, d *dev.Device) error {
		d.Ready(memDisk(img))
		<-ctx.Done()
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := dev.NewManager(proc.NewTable(), nil, nil)
	m.Start(ctx, []dev.Node{{Name: "nvme0", Compatible: "test,disk"}})
	if _, err := m.Get(ctx, "nvme0"); err != nil {
		t.Fatal(err)
	}
	out := &syncBuf{}
	term := tty.New(make(chan byte), out, false)
	defer term.Close()
	ns := vfs.New(ctx, m)
	ns.Mount("ram", memfs.New("ram"))
	env := Env{TTY: term, Dev: m, FS: ns}

	run := func(f func(context.Context, Env, *proc.Proc, []string) error, args ...string) string {
		t.Helper()
		before := len(out.String())
		if err := f(ctx, env, nil, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()[before:]
	}
	s := run(lsblk)
	for _, want := range []string{"nvme0 ", "64M", "disk (gpt)", "nvme0p1", "FAT32", "GODEEPFAT", "nvme0p2", "Linux data", "ext4", "godeep-ext4"} {
		if !strings.Contains(s, want) {
			t.Fatalf("lsblk lacks %q:\n%s", want, s)
		}
	}
	if s := run(cat, "nvme0p2:/hello.txt", "nvme0p1:docs/readme.md"); s != "hello from godeep\n# docs\nread-only for now.\n" {
		t.Fatalf("cat: %q", s)
	}
	s = run(ls, "nvme0p2:/")
	for _, want := range []string{"docs/", "link-to-hello@", "big.bin", "195K"} {
		if !strings.Contains(s, want) {
			t.Fatalf("ls lacks %q:\n%s", want, s)
		}
	}
	if s := run(ls, "nvme0p1:/many/f123.txt"); !strings.Contains(s, "f123.txt") {
		t.Fatalf("ls of a file: %q", s)
	}
	for _, bad := range [][]string{{"nvme0p9:/"}, {"sda:/"}, {"nvme0"}, {"nvme0p2:/nope"}} {
		if err := ls(ctx, env, nil, bad); err == nil {
			t.Errorf("ls %v: no error", bad)
		}
	}
	if !bytes.Contains([]byte(out.String()), []byte("drwx")) {
		t.Fatal("no directory modes in ls output")
	}

	// ram: is writable; disks aren't.
	run(cp, "nvme0p2:/hello.txt", "ram:/hello.txt")
	run(mkdir, "ram:/d")
	run(write, "ram:/d/note", "hi", "there")
	run(mv, "ram:/d/note", "ram:/note.txt")
	if s := run(cat, "ram:/hello.txt", "ram:note.txt"); s != "hello from godeep\nhi there\n" {
		t.Fatalf("ram cat: %q", s)
	}
	if s := run(ls, "ram:/"); !strings.Contains(s, "d/") || !strings.Contains(s, "note.txt") {
		t.Fatalf("ls ram: %q", s)
	}
	run(rm, "ram:/d")
	if s := run(df); !strings.Contains(s, "ram") || !strings.Contains(s, "rw") || !strings.Contains(s, "nvme0p2") {
		t.Fatalf("df: %q", s)
	}
	if err := write(ctx, env, nil, []string{"nvme0p2:/x", "no"}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("write to a disk: %v", err)
	}
	if err := mv(ctx, env, nil, []string{"ram:/note.txt", "nvme0p2:/x"}); err == nil {
		t.Fatal("mv across volumes")
	}
}
