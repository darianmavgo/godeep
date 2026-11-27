// fsckpart checks a filesystem inside a disk image with the host's own
// checker: fsckpart IMAGE N extracts partition N (GPT or MBR, as godeep
// reads it) and runs `e2fsck -fn` (ext2/3/4) or `fsck_msdos -n` (FAT) on
// it. make check-virt uses it after godeep wrote to bin/disk-rw.img.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"godeep/kernel/block"
)

// image is a disk image file as a block.Device.
type image struct {
	f    *os.File
	size int64
}

func (i image) BlockSize() int { return 512 }
func (i image) Blocks() uint64 { return uint64(i.size / 512) }
func (i image) ReadBlocks(_ context.Context, lba uint64, p []byte) error {
	_, err := i.f.ReadAt(p, int64(lba)*512)
	return err
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: fsckpart IMAGE PARTITION")
		os.Exit(2)
	}
	n, err := strconv.Atoi(os.Args[2])
	check(err)
	f, err := os.Open(os.Args[1])
	check(err)
	defer f.Close()
	st, err := f.Stat()
	check(err)
	ctx := context.Background()
	parts, _, err := block.Partitions(ctx, image{f, st.Size()})
	check(err)
	var p *block.Partition
	for _, x := range parts {
		if x.Index == n {
			p = x
		}
	}
	if p == nil {
		check(fmt.Errorf("no partition %d", n))
	}
	data := make([]byte, p.Count*512)
	_, err = f.ReadAt(data, int64(p.Start)*512)
	check(err)
	dir, err := os.MkdirTemp("", "fsckpart")
	check(err)
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "part.img")
	check(os.WriteFile(path, data, 0o644))

	var cmd *exec.Cmd
	switch {
	case len(data) > 1080 && data[1080] == 0x53 && data[1081] == 0xef:
		bin := "/opt/homebrew/opt/e2fsprogs/sbin/e2fsck"
		if _, err := os.Stat(bin); err != nil {
			bin = "e2fsck"
		}
		cmd = exec.Command(bin, "-fn", path)
	default:
		cmd = exec.Command("fsck_msdos", "-n", path)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fsckpart: %s partition %d: %v\n%s", os.Args[1], n, err, out)
		os.Exit(1)
	}
	fmt.Printf("fsckpart: %s partition %d: clean (%s)\n", os.Args[1], n, filepath.Base(cmd.Path))
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fsckpart:", err)
		os.Exit(1)
	}
}
