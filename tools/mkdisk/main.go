// mkdisk builds bin/disk.img, the NVMe disk QEMU virt gets (make qemu):
// a GPT disk with the FAT32 and ext4 test images (fs/testdata) as its two
// partitions, so lsblk, ls and cat have something to read.
package main

import (
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"godeep/fs/fat"
	"godeep/kernel/block"
)

// puts collects -put PATH=HOSTFILE flags.
type puts []string

func (p *puts) String() string     { return strings.Join(*p, ",") }
func (p *puts) Set(v string) error { *p = append(*p, v); return nil }

// mem is a disk image in memory, read and written through fs/fat.
type mem []byte

func (m mem) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m)) {
		return 0, io.EOF
	}
	n := copy(p, m[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m mem) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > int64(len(m)) {
		return 0, io.ErrShortWrite
	}
	return copy(m[off:], p), nil
}

// putFiles copies host files into a FAT image (directories made as needed).
func putFiles(img []byte, list []string) error {
	f, err := fat.NewWritable(mem(img), mem(img))
	if err != nil {
		return err
	}
	for _, p := range list {
		dst, src, ok := strings.Cut(p, "=")
		if !ok {
			return fmt.Errorf("-put %q: want PATH=HOSTFILE", p)
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		dst = strings.TrimPrefix(dst, "/")
		parts := strings.Split(dst, "/")
		for i := 1; i < len(parts); i++ {
			f.Mkdir(strings.Join(parts[:i], "/")) // may exist
		}
		if err := f.WriteFile(dst, b); err != nil {
			return fmt.Errorf("-put %s: %w", dst, err)
		}
	}
	return nil
}

func gunzip(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(z)
}

func main() {
	out := flag.String("o", "bin/disk.img", "output image")
	data := flag.String("testdata", "fs/testdata", "directory with the filesystem images")
	size := flag.Int64("size", 64<<20, "disk size in bytes")
	mac := flag.Bool("mac", false, "a disk laid out like the Mac's SSD, for the write fence: an APFS stand-in, the FAT32 test image labelled GODEEPDATA, and the same image labelled OTHER")
	var put puts
	flag.Var(&put, "put", "PATH=HOSTFILE: copy a file into the first FAT32 partition (GODEEPDATA with -mac); repeatable")
	flag.Parse()

	type spec struct{ file, name, typ, label string }
	specs := []spec{
		{"fat32.img.gz", "GODEEPFAT", block.TypeBasicData, ""},
		{"ext4.img.gz", "godeep-ext4", block.TypeLinux, ""},
	}
	if *mac {
		specs = []spec{
			{"", "Macintosh HD", block.TypeAPFS, ""},
			{"fat32.img.gz", "", block.TypeBasicData, "GODEEPDATA"},
			{"fat32.img.gz", "", block.TypeBasicData, "OTHER"},
		}
	}
	var parts []block.PartSpec
	for _, p := range specs {
		b := make([]byte, 1<<20) // the APFS stand-in: 1 MiB of 0xaa
		for i := range b {
			b[i] = 0xaa
		}
		if p.file != "" {
			var err error
			if b, err = gunzip(filepath.Join(*data, p.file)); err != nil {
				fmt.Fprintln(os.Stderr, "mkdisk:", err)
				os.Exit(1)
			}
		}
		if p.label != "" { // FAT32 boot sector: volume label at 0x47
			copy(b[0x47:0x52], fmt.Sprintf("%-11s", p.label))
		}
		if p.file == "fat32.img.gz" && len(put) > 0 {
			if err := putFiles(b, put); err != nil {
				fmt.Fprintln(os.Stderr, "mkdisk:", err)
				os.Exit(1)
			}
			put = nil // the first FAT32 partition only
		}
		parts = append(parts, block.PartSpec{Name: p.name, Type: p.typ, Data: b})
	}
	img, err := block.MakeGPT(*size, 512, parts)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(*out), 0o755)
	}
	if err == nil {
		err = os.WriteFile(*out, img, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkdisk:", err)
		os.Exit(1)
	}
	if *mac {
		fmt.Printf("mkdisk: %s: %d MiB, GPT: 1 APFS stand-in, 2 FAT32 GODEEPDATA, 3 FAT32 OTHER\n", *out, *size>>20)
	} else {
		fmt.Printf("mkdisk: %s: %d MiB, GPT: 1 GODEEPFAT (FAT32), 2 godeep-ext4 (ext4)\n", *out, *size>>20)
	}
}
