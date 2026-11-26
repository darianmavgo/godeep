package main

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"godeep/fs/ninep"
)

func mount(t *testing.T, dir string, ro bool) *ninep.Client {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	go Serve(a, root, ro)
	t.Cleanup(func() { b.Close() })
	tr, err := ninep.DialTCP(context.Background(), func(context.Context, string, string) (net.Conn, error) { return b, nil }, "pipe", 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ninep.Mount(context.Background(), tr, "dev")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestServeReadWrite(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src/pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "src/pkg/a.go"), []byte("package pkg\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 300<<10), 0o644)
	c := mount(t, dir, false)
	if !c.Writable() {
		t.Fatal("writable share probed read-only")
	}
	if _, err := os.Stat(filepath.Join(dir, ".godeep-write-probe")); !os.IsNotExist(err) {
		t.Fatal("write probe left behind")
	}
	if err := fstest.TestFS(c, "src/pkg/a.go", "big.bin"); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Mkdir("out"))
	must(c.WriteFile("out/result.txt", make([]byte, 200<<10))) // several Twrites
	must(c.WriteFile("out/result.txt", []byte("written by godeep\n")))
	if _, err := c.WriteAt("out/result.txt", []byte("more\n"), 18); err != nil {
		t.Fatal(err)
	}
	must(c.Rename("out/result.txt", "out/final.txt"))
	must(c.Truncate("big.bin", 10))
	b, err := os.ReadFile(filepath.Join(dir, "out/final.txt"))
	if err != nil || string(b) != "written by godeep\nmore\n" {
		t.Fatalf("on disk: %q %v", b, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "big.bin")); fi.Size() != 10 {
		t.Fatal("truncate didn't reach the disk")
	}
	if err := c.Remove("out"); err == nil {
		t.Fatal("removed a non-empty directory")
	}
	must(c.Remove("out/final.txt"))
	must(c.Remove("out"))
	if _, err := c.Stat("../" + filepath.Base(dir)); err == nil {
		t.Fatal("escaped the root")
	}
}

func TestServeReadOnly(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o644)
	c := mount(t, dir, true)
	if c.Writable() {
		t.Fatal("read-only share probed writable")
	}
	if err := c.WriteFile("b", nil); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("write to a read-only share: %v", err)
	}
	if b, err := fs.ReadFile(c, "a"); err != nil || string(b) != "a" {
		t.Fatalf("read: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".godeep-write-probe")); !os.IsNotExist(err) {
		t.Fatal("probe file created on a read-only share")
	}
}
