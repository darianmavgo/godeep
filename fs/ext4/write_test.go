package ext4

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// memImage is a disk image in memory: ReaderAt and WriterAt.
type memImage []byte

func (m memImage) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m)) {
		return 0, io.EOF
	}
	n := copy(p, m[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m memImage) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(m)) {
		return 0, errors.New("write past the end of the image")
	}
	return copy(m[off:], p), nil
}

func writable(t *testing.T, name string) (*FS, memImage) {
	t.Helper()
	b, err := io.ReadAll(image(t, name))
	if err != nil {
		t.Fatal(err)
	}
	m := memImage(b)
	f, err := NewWritable(m, m)
	if err != nil {
		t.Fatal(err)
	}
	return f, m
}

// e2fsck runs `e2fsck -fn` on the image: it must find nothing to fix.
func e2fsck(t *testing.T, m memImage) {
	t.Helper()
	bin := "/opt/homebrew/opt/e2fsprogs/sbin/e2fsck"
	if _, err := os.Stat(bin); err != nil {
		if p, err := exec.LookPath("e2fsck"); err == nil {
			bin = p
		} else {
			t.Log("no e2fsck (brew install e2fsprogs): consistency not checked")
			return
		}
	}
	path := filepath.Join(t.TempDir(), "fs.img")
	if err := os.WriteFile(path, m, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "-fn", path).CombinedOutput()
	if err != nil {
		t.Fatalf("e2fsck -fn: %v\n%s", err, out)
	}
}

func readFile(t *testing.T, f *FS, name string) string {
	t.Helper()
	b, err := fs.ReadFile(f, name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(b)
}

func TestWriteBasics(t *testing.T) {
	f, m := writable(t, "ext4.img.gz")
	if !f.Writable() {
		t.Fatalf("not writable: %s", f.wr.why)
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 40000) // 640 KB: many blocks
	steps := []struct {
		name string
		do   func() error
	}{
		{"new file", func() error { return f.WriteFile("new.txt", []byte("written by godeep\n")) }},
		{"overwrite", func() error { return f.WriteFile("hello.txt", []byte("replaced\n")) }},
		{"big file", func() error { return f.WriteFile("docs/big.dat", big) }},
		{"mkdir", func() error { return f.Mkdir("made") }},
		{"nested", func() error { return f.WriteFile("made/inside.txt", []byte("in a new dir\n")) }},
		{"write at", func() error { _, err := f.WriteAt("new.txt", []byte("GODEEP"), 11); return err }},
		{"truncate", func() error { return f.Truncate("docs/big.dat", 100000) }},
		{"into htree dir", func() error { return f.WriteFile("many/f301.txt", []byte("file 301\n")) }},
		{"remove from htree dir", func() error { return f.Remove("many/f001.txt") }},
		{"rename file", func() error { return f.Rename("made/inside.txt", "docs/moved.txt") }},
		{"rename dir", func() error { return f.Rename("made", "deep/made") }},
		{"remove empty dir", func() error { return f.Remove("empty-dir") }},
		{"remove symlink", func() error { return f.Remove("link-to-hello") }},
		{"create", func() error {
			w, err := f.Create("created.txt")
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "line one\nline two\n")
			return w.Close()
		}},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
	}
	for name, want := range map[string]string{
		"new.txt":        "written by GODEEP\n",
		"hello.txt":      "replaced\n",
		"many/f301.txt":  "file 301\n",
		"docs/moved.txt": "in a new dir\n",
		"created.txt":    "line one\nline two\n",
		"docs/readme.md": "# docs\nread-only for now.\n",
	} {
		if got := readFile(t, f, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := readFile(t, f, "docs/big.dat"); got != string(big[:100000]) {
		t.Errorf("big.dat: %d bytes", len(got))
	}
	if got := readFile(t, f, "big.bin"); len(got) != 200000 || got[12345] != bigByte(12345) {
		t.Error("big.bin changed")
	}
	ents, _ := fs.ReadDir(f, "many")
	if len(ents) != 300 {
		t.Errorf("many/ has %d entries, want 300", len(ents))
	}
	if _, err := fs.Stat(f, "deep/made"); err != nil {
		t.Errorf("moved dir: %v", err)
	}
	for _, gone := range []string{"many/f001.txt", "empty-dir", "made", "link-to-hello"} {
		if _, err := fs.Stat(f, gone); err == nil {
			t.Errorf("%s still there", gone)
		}
	}
	// errors
	for name, err := range map[string]error{
		"mkdir exists":     f.Mkdir("docs"),
		"remove non-empty": f.Remove("docs"),
		"remove missing":   f.Remove("nope"),
		"rename onto":      f.Rename("new.txt", "hello.txt"),
		"dir into itself":  f.Rename("deep", "deep/x/y"),
		"file as dir":      f.WriteFile("hello.txt/x", nil),
		"write a dir":      f.WriteFile("docs", nil),
	} {
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	e2fsck(t, m)

	// It reopens cleanly.
	g, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, g, "hello.txt"); got != "replaced\n" {
		t.Fatalf("reopened: %q", got)
	}
}

// TestWriteRandom: random operations against a model, then e2fsck.
func TestWriteRandom(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { writeRandom(t, seed) })
	}
}

func writeRandom(t *testing.T, seed int64) {
	f, m := writable(t, "ext4.img.gz")
	rng := rand.New(rand.NewSource(seed))
	files := map[string][]byte{}
	dirs := []string{"."}
	written, full := 0, 0
	for i := 0; i < 400; i++ {
		dir := dirs[rng.Intn(len(dirs))]
		name := strings.TrimPrefix(fmt.Sprintf("%s/n%d", dir, rng.Intn(30)), "./")
		switch op := rng.Intn(10); {
		case op < 5:
			data := bytes.Repeat([]byte{byte(i)}, rng.Intn(20000))
			if err := f.WriteFile(name, data); err == nil {
				files[name] = data
				written++
			} else if _, isFile := files[name]; isFile && !errors.Is(err, ErrNoSpace) {
				t.Fatalf("write %s: %v", name, err)
			} else if errors.Is(err, ErrNoSpace) {
				full++ // the old contents stay (checked at the end)
			}
		case op < 7:
			if _, ok := files[name]; ok {
				if err := f.Remove(name); err != nil {
					t.Fatalf("remove %s: %v", name, err)
				}
				delete(files, name)
			}
		case op < 8:
			if _, ok := files[name]; !ok && f.Mkdir(name) == nil {
				dirs = append(dirs, name)
			}
		default:
			if data, ok := files[name]; ok {
				to := name + "r"
				if _, exists := files[to]; !exists && f.Rename(name, to) == nil {
					delete(files, name)
					files[to] = data
				}
			}
		}
	}
	for name, data := range files {
		if got := readFile(t, f, name); got != string(data) {
			t.Fatalf("%s: %d bytes, want %d", name, len(got), len(data))
		}
	}
	t.Logf("%d writes, %d refused for space, %d files at the end", written, full, len(files))
	if full == 0 && seed == 1 {
		t.Error("the disk never filled up: the full-disk paths weren't exercised")
	}
	e2fsck(t, m)
}

func TestNotWritable(t *testing.T) {
	f, _ := writable(t, "ext2.img.gz") // block maps
	if f.Writable() {
		t.Fatal("ext2 claims to be writable")
	}
	if err := f.WriteFile("x", nil); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("write: %v", err)
	}
	g, err := New(image(t, "ext4.img.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if g.Writable() || g.Mkdir("x") == nil {
		t.Fatal("read-only open writes")
	}
}
