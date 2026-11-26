package memfs

import (
	"errors"
	"io"
	"io/fs"
	"sync"
	"testing"
	"testing/fstest"
)

func TestReadWrite(t *testing.T) {
	f := New("ram")
	if err := f.Mkdir("docs"); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("docs/a.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	w, err := f.Create("b.bin")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "abc")
	io.WriteString(w, "def")
	w.Close()
	if err := fstest.TestFS(f, "docs/a.txt", "b.bin", "docs"); err != nil {
		t.Fatal(err)
	}
	if b, _ := fs.ReadFile(f, "b.bin"); string(b) != "abcdef" {
		t.Fatalf("b.bin %q", b)
	}
	if f.Used() != 11 {
		t.Fatalf("used %d", f.Used())
	}
	if err := f.Rename("b.bin", "docs/c.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Stat("b.bin"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("old name: %v", err)
	}
	if err := f.Remove("docs"); err == nil {
		t.Fatal("removed a non-empty directory")
	}
	f.Remove("docs/a.txt")
	f.Remove("docs/c.bin")
	if err := f.Remove("docs"); err != nil || f.Used() != 0 {
		t.Fatalf("remove: %v, used %d", err, f.Used())
	}
	if err := f.Truncate("nope", 3); err == nil {
		t.Fatal("truncate of a missing file")
	}
}

func TestLimitAndBadPaths(t *testing.T) {
	f := New("ram")
	f.Limit = 10
	if err := f.WriteFile("a", make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("b", make([]byte, 3)); !errors.Is(err, ErrFull) {
		t.Fatalf("over the limit: %v", err)
	}
	if err := f.WriteFile("a", make([]byte, 10)); err != nil {
		t.Fatalf("replacing within the limit: %v", err)
	}
	for _, p := range []string{"../x", "a/../b", "x//y", "/abs", ""} {
		if err := f.WriteFile(p, nil); err == nil {
			t.Fatalf("bad path %q accepted", p)
		}
	}
	if err := f.WriteFile("missing/x", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing parent: %v", err)
	}
	if err := f.Rename(".", "x"); err == nil {
		t.Fatal("renamed the root")
	}
	f.Mkdir("d")
	if err := f.Rename("d", "d/e"); err == nil {
		t.Fatal("moved a directory into itself")
	}
}

func TestConcurrent(t *testing.T) {
	f := New("ram")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := string(rune('a' + i))
			for j := 0; j < 200; j++ {
				f.WriteFile(name, []byte{byte(j)})
				fs.ReadFile(f, name)
				fs.ReadDir(f, ".")
			}
		}()
	}
	wg.Wait()
	if f.Used() != 8 {
		t.Fatalf("used %d", f.Used())
	}
}
