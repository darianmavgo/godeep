package sh

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"godeep/fs/memfs"
	"godeep/fs/ninep"
	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/proc"
	"godeep/kernel/tty"
	"godeep/kernel/vfs"
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

type dialNet struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (d dialNet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.dial(ctx, network, addr)
}
func (d dialNet) Listen(context.Context, string, string) (net.Listener, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestMount9pAndUmount(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("from host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuf{}
	term := tty.New(make(chan byte), out, false)
	defer term.Close()
	ns := vfs.New(ctx, nil)
	ns.Mount("ram", memfs.New("ram"))
	env := Env{TTY: term, FS: ns, Net: dialNet{dial: func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		go ninep.Serve(a, root, false)
		return b, nil
	}}}

	// Mount using a short-lived command context (like ssh exec): the volume
	// must stay usable after cmdCtx is cancelled.
	cmdCtx, cmdCancel := context.WithCancel(ctx)
	if err := mount9pCmd(cmdCtx, env, nil, []string{"10.0.2.2:5640", "dev"}); err != nil {
		t.Fatalf("mount9p: %v", err)
	}
	cmdCancel()
	if !strings.Contains(out.String(), "mounted 10.0.2.2:5640 as dev: (rw)") {
		t.Fatalf("mount9p output: %q", out.String())
	}
	if err := cat(ctx, env, nil, []string{"dev:/seed.txt"}); err != nil {
		t.Fatalf("cat dev: %v", err)
	}
	if err := write(ctx, env, nil, []string{"dev:/guest.txt", "hello", "9p"}); err != nil {
		t.Fatalf("write dev: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "guest.txt"))
	if err != nil || string(got) != "hello 9p\n" {
		t.Fatalf("host file: %q %v", got, err)
	}
	before := len(out.String())
	if err := df(ctx, env, nil, nil); err != nil {
		t.Fatal(err)
	}
	if s := out.String()[before:]; !strings.Contains(s, "dev") || !strings.Contains(s, "9p") || !strings.Contains(s, "rw") {
		t.Fatalf("df missing dev: %q", s)
	}
	if err := umount(ctx, env, nil, []string{"ram"}); err == nil {
		t.Fatal("umount ram should fail")
	}
	if err := umount(ctx, env, nil, []string{"dev"}); err != nil {
		t.Fatalf("umount dev: %v", err)
	}
	if err := cat(ctx, env, nil, []string{"dev:/seed.txt"}); err == nil {
		t.Fatal("cat after umount should fail")
	}
}

func TestIsEnvAssign(t *testing.T) {
	for _, tc := range []struct {
		tok  string
		want bool
	}{
		{"FOO=bar", true},
		{"_A1=/dev/out.txt", true},
		{"EMPTY=", true},
		{"1BAD=x", false},
		{"=val", false},
		{"ram:/hello.wasm", false},
		{"write", false},
	} {
		if got := isEnvAssign(tc.tok); got != tc.want {
			t.Errorf("isEnvAssign(%q) = %v, want %v", tc.tok, got, tc.want)
		}
	}
}
