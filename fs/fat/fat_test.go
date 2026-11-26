package fat

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"testing"
	"testing/fstest"
)

func image(t *testing.T, name string) *bytes.Reader {
	t.Helper()
	f, err := os.Open("../testdata/" + name)
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
	return bytes.NewReader(b)
}

// bigByte is byte i of big.bin (fs/testdata/README.md).
func bigByte(i int) byte { return byte((i*7 + i/251) % 256) }

func TestImages(t *testing.T) {
	for _, c := range []struct{ img, typ string }{{"fat12.img.gz", "FAT12"}, {"fat16.img.gz", "FAT16"}, {"fat32.img.gz", "FAT32"}} {
		t.Run(c.typ, func(t *testing.T) {
			f, err := New(image(t, c.img))
			if err != nil {
				t.Fatal(err)
			}
			if f.Type() != c.typ || f.Label() != "GODEEPFAT" {
				t.Fatalf("type %s label %q", f.Type(), f.Label())
			}
			for name, want := range map[string]string{
				"hello.txt":            "hello from godeep\n",
				"docs/readme.md":       "# docs\nread-only for now.\n",
				"A Long File Name.txt": "long names work\n",
				"a long file name.TXT": "long names work\n", // case-insensitive
				"deep/a/b/c.txt":       "deep\n",
				"empty.txt":            "",
				"many/f300.txt":        "file 300\n",
			} {
				got, err := fs.ReadFile(f, name)
				if err != nil || string(got) != want {
					t.Errorf("%s: %q, %v", name, got, err)
				}
			}
			big, err := fs.ReadFile(f, "big.bin")
			if err != nil || len(big) != 200000 {
				t.Fatalf("big.bin: %d bytes, %v", len(big), err)
			}
			for i, b := range big {
				if b != bigByte(i) {
					t.Fatalf("big.bin byte %d", i)
				}
			}
			many, err := fs.ReadDir(f, "many")
			if err != nil || len(many) != 300 {
				t.Fatalf("many/: %d entries, %v", len(many), err)
			}
			if _, err := fs.Stat(f, "nope.txt"); err == nil {
				t.Fatal("missing file found")
			}
			if err := fstest.TestFS(f, "hello.txt", "docs/readme.md", "deep/a/b/c.txt", "big.bin", "many/f001.txt"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNotFAT(t *testing.T) {
	if Probe(bytes.NewReader(make([]byte, 4096))) {
		t.Fatal("zeros taken for FAT")
	}
	if Probe(image(t, "ext4.img.gz")) {
		t.Fatal("ext4 taken for FAT")
	}
}
