package fat

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// memImage is an image in memory, readable and writable at any offset.
type memImage struct{ b []byte }

func (m *memImage) ReadAt(p []byte, off int64) (int, error)  { return copy(p, m.b[off:]), nil }
func (m *memImage) WriteAt(p []byte, off int64) (int, error) { return copy(m.b[off:], p), nil }

func writable(t *testing.T, img string) (*FS, *memImage) {
	t.Helper()
	r := image(t, img)
	b := make([]byte, r.Len())
	r.ReadAt(b, 0)
	m := &memImage{b}
	f, err := NewWritable(m, m)
	if err != nil || !f.Writable() {
		t.Fatalf("NewWritable: %v", err)
	}
	return f, m
}

// fsck runs macOS's FAT checker on the image, if it's there.
func fsck(t *testing.T, m *memImage) {
	t.Helper()
	const tool = "/sbin/fsck_msdos"
	if _, err := os.Stat(tool); err != nil {
		t.Log("no fsck_msdos: skipping the independent check")
		return
	}
	p := filepath.Join(t.TempDir(), "fat.img")
	os.WriteFile(p, m.b, 0o644)
	out, err := exec.Command(tool, "-n", p).CombinedOutput()
	if err != nil || bytes.Contains(out, []byte("Fix?")) || bytes.Contains(out, []byte("CORRECT")) {
		t.Fatalf("fsck_msdos: %v\n%s", err, out)
	}
}

func TestWrite(t *testing.T) {
	for _, img := range []string{"fat16.img.gz", "fat32.img.gz"} {
		t.Run(img, func(t *testing.T) {
			f, m := writable(t, img)
			big := bytes.Repeat([]byte("0123456789abcdef"), 40000) // 640 KB: many clusters
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(f.WriteFile("new.txt", []byte("hello from godeep\n")))
			must(f.Mkdir("Out Dir"))
			must(f.WriteFile("Out Dir/A Long File Name.bin", big))
			must(f.WriteFile("new.txt", []byte("shorter\n"))) // overwrite
			if _, err := f.WriteAt("new.txt", []byte("appended\n"), 8); err != nil {
				t.Fatal(err)
			}
			w, err := f.Create("Out Dir/streamed.log")
			must(err)
			for i := 0; i < 1000; i++ {
				fmt.Fprintf(w, "line %d\n", i)
			}
			for i := 0; i < 300; i++ { // grows the directory past its first cluster
				must(f.WriteFile(fmt.Sprintf("Out Dir/file number %03d.txt", i), []byte{byte(i)}))
			}
			must(f.Mkdir("moved"))
			must(f.Mkdir("Out Dir/sub"))
			must(f.WriteFile("Out Dir/sub/x", []byte("x")))
			must(f.Rename("Out Dir/sub", "moved/sub")) // a directory changes parents
			must(f.Truncate("Out Dir/A Long File Name.bin", 100000))
			must(f.WriteFile("gone", []byte("bye")))
			must(f.Remove("gone"))
			if err := f.Remove("moved"); err == nil {
				t.Fatal("removed a non-empty directory")
			}
			if err := f.WriteFile("bad:name", nil); err == nil {
				t.Fatal("bad name accepted")
			}

			// Reopen from scratch: everything is on the "disk".
			g, err := New(m)
			must(err)
			check := func(name, want string) {
				t.Helper()
				b, err := fs.ReadFile(g, name)
				if err != nil || string(b) != want {
					t.Fatalf("%s: %q %v", name, b, err)
				}
			}
			check("new.txt", "shorter\nappended\n")
			check("moved/sub/x", "x")
			check("Out Dir/file number 299.txt", "\x2b")
			b, _ := fs.ReadFile(g, "Out Dir/A Long File Name.bin")
			if !bytes.Equal(b, big[:100000]) {
				t.Fatalf("big file: %d bytes", len(b))
			}
			log, _ := fs.ReadFile(g, "Out Dir/streamed.log")
			if !strings.HasPrefix(string(log), "line 0\nline 1\n") || !strings.HasSuffix(string(log), "line 999\n") {
				t.Fatalf("streamed: %d bytes", len(log))
			}
			if _, err := fs.Stat(g, "gone"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("removed file: %v", err)
			}
			if b, _ := fs.ReadFile(g, "hello.txt"); len(b) == 0 { // the image's own files survive
				t.Fatal("hello.txt lost")
			}
			must(fstest.TestFS(g, "new.txt", "moved/sub/x", "Out Dir/streamed.log"))
			fsck(t, m)
		})
	}
}

func TestFAT12StaysReadOnly(t *testing.T) {
	r := image(t, "fat12.img.gz")
	f, err := NewWritable(r, nil)
	if err != nil || f.Writable() {
		t.Fatalf("FAT12 writable: %v", err)
	}
	if err := f.WriteFile("x", nil); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("write: %v", err)
	}
}
