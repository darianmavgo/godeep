package synth

import (
	"io/fs"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

func TestFS(t *testing.T) {
	var n atomic.Int64
	counter := Text(func() string { return strconv.FormatInt(n.Add(1), 10) + "\n" })
	root := Static(map[string]*Node{
		"version": Text(func() string { return "godeep test\n" }),
		"sub": Static(map[string]*Node{
			"hello": Text(func() string { return "hi\n" }),
		}),
		"dyn": {
			List: func() []string { return []string{"1", "2"} },
			Get: func(name string) *Node {
				if name == "1" || name == "2" {
					return Static(map[string]*Node{"name": Text(func() string { return "p" + name + "\n" })})
				}
				return nil
			},
		},
	})
	f := New("synth", "test", root)
	if err := fstest.TestFS(f, "version", "sub/hello", "dyn/1/name", "dyn/2/name"); err != nil {
		t.Fatal(err)
	}
	if b, _ := fs.ReadFile(f, "dyn/2/name"); string(b) != "p2\n" {
		t.Fatalf("dyn/2/name = %q", b)
	}
	if _, err := f.Open("dyn/3"); err == nil {
		t.Fatal("dyn/3 exists")
	}
	if _, err := f.Open("version/x"); err == nil {
		t.Fatal("file has children")
	}
	g := New("synth", "c", Static(map[string]*Node{"c": counter}))
	a, _ := fs.ReadFile(g, "c")
	b, _ := fs.ReadFile(g, "c")
	if string(a) == string(b) {
		t.Fatal("contents not generated per open")
	}
}
