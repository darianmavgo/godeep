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

	"godeep/kernel/block"
)

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
	flag.Parse()

	var parts []block.PartSpec
	for _, p := range []struct{ file, name, typ string }{
		{"fat32.img.gz", "GODEEPFAT", block.TypeBasicData},
		{"ext4.img.gz", "godeep-ext4", block.TypeLinux},
	} {
		b, err := gunzip(filepath.Join(*data, p.file))
		if err != nil {
			fmt.Fprintln(os.Stderr, "mkdisk:", err)
			os.Exit(1)
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
	fmt.Printf("mkdisk: %s: %d MiB, GPT: 1 GODEEPFAT (FAT32), 2 godeep-ext4 (ext4)\n", *out, *size>>20)
}
