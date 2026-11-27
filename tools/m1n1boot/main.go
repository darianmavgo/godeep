// Command m1n1boot makes m1n1 boot godeep directly, without U-Boot.
//
// An Asahi boot partition holds m1n1/boot.bin, which is m1n1 stage 2 followed
// by its payloads, concatenated:
//
//	m1n1.bin | [config lines] | *.dtb | gzip(u-boot-nodtb.bin)
//
// m1n1 walks the payloads, picks the device tree for this Mac and boots the
// last one, a kernel Image (U-Boot is built as one). This tool keeps
// everything before that gzipped Image and puts gzip(godeep Image) in its place,
// so choosing "godeep" at startup boots godeep no matter what U-Boot would have
// scanned (on a Mac with Omarchy, U-Boot finds Omarchy's ESP first).
//
//	go run ./tools/m1n1boot -base boot.bin.uboot -kernel bin/godeep-applem1.img -o boot.bin
//	go run ./tools/m1n1boot -info boot.bin
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	imageMagic    = 0x644d5241 // "ARM\x64" at offset 0x38 of an arm64 Image
	maxPayload    = 256 << 20  // decompression limit when probing
	imageHdrBytes = 64
)

func main() {
	base := flag.String("base", "", "m1n1 boot.bin whose last payload is a gzipped kernel Image (U-Boot)")
	kernel := flag.String("kernel", "", "godeep arm64 Image (bin/godeep-applem1.img)")
	out := flag.String("o", "", "output boot.bin")
	info := flag.String("info", "", "describe a boot.bin's kernel payload and exit")
	bootargs := flag.String("bootargs", "", "kernel arguments for /chosen/bootargs (e.g. godeep.nosmp): a chosen.bootargs= line m1n1 reads before the kernel")
	flag.Parse()

	if *info != "" {
		data, err := os.ReadFile(*info)
		check(err)
		p, err := findKernel(data)
		check(err)
		fmt.Printf("%s: m1n1 + payloads %d bytes, kernel payload at %#x: %d bytes gzipped, %d bytes Image, %s\n",
			*info, p.Off, p.Off, p.Compressed, len(p.Image), identify(p.Image))
		return
	}
	if *base == "" || *kernel == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	b, err := os.ReadFile(*base)
	check(err)
	k, err := os.ReadFile(*kernel)
	check(err)
	res, kept, err := build(b, k, *bootargs)
	check(err)
	check(os.WriteFile(*out, res, 0o644))
	fmt.Printf("%s: m1n1 + payloads from %s (%d bytes) + gzip(%s) = %d bytes\n", *out, *base, kept, *kernel, len(res))
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "m1n1boot:", err)
		os.Exit(1)
	}
}

// payload is the gzipped kernel Image at the end of a boot.bin.
type payload struct {
	Off        int    // where the gzip stream starts (= length of what's kept)
	Compressed int    // gzip stream length
	Image      []byte // decompressed
}

// findKernel finds the first gzip stream that decompresses to an arm64 Image
// and runs to the end of the file (only zero padding may follow).
func findKernel(data []byte) (payload, error) {
	for i := 0; i+3 <= len(data); i++ {
		if data[i] != 0x1f || data[i+1] != 0x8b || data[i+2] != 0x08 {
			continue
		}
		r := bytes.NewReader(data[i:]) // a ByteReader, so gzip reads no further than its stream
		zr, err := gzip.NewReader(r)
		if err != nil {
			continue
		}
		zr.Multistream(false)
		img, err := io.ReadAll(io.LimitReader(zr, maxPayload))
		if err != nil || !isImage(img) {
			continue
		}
		end := len(data) - r.Len()
		if !allZero(data[end:]) {
			continue
		}
		return payload{Off: i, Compressed: end - i, Image: img}, nil
	}
	return payload{}, errors.New("no gzipped arm64 kernel Image at the end of this boot.bin (is it m1n1's?)")
}

func isImage(b []byte) bool {
	return len(b) >= imageHdrBytes && binary.LittleEndian.Uint32(b[0x38:]) == imageMagic
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// identify tells U-Boot from godeep by a string each carries.
func identify(img []byte) string {
	switch {
	case bytes.Contains(img, []byte("godeep")):
		return "godeep"
	case bytes.Contains(img, []byte("U-Boot")):
		return "U-Boot"
	}
	return "unknown kernel"
}

// build returns base with its kernel payload replaced by gzip(kernel),
// preceded by a "chosen.bootargs=ARGS" line if bootargs is set (m1n1 sets
// /chosen properties from chosen.NAME=VALUE lines among its payloads).
// It also returns how many bytes of base (m1n1 and its other payloads) it kept.
func build(base, kernel []byte, bootargs string) ([]byte, int, error) {
	if strings.ContainsAny(bootargs, "\n\x00") {
		return nil, 0, errors.New("bootargs: one line, no NUL")
	}
	if !isImage(kernel) {
		return nil, 0, errors.New("kernel is not an arm64 Image (build it with make image BOARD=applem1)")
	}
	p, err := findKernel(base)
	if err != nil {
		return nil, 0, err
	}
	if !hasFDT(base[:p.Off]) {
		return nil, 0, errors.New("no device tree before the kernel payload; m1n1 would have nothing to pass godeep")
	}
	var out bytes.Buffer
	out.Write(base[:p.Off])
	if bootargs != "" {
		out.WriteString("chosen.bootargs=" + bootargs + "\n")
	}
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if _, err := zw.Write(kernel); err != nil {
		return nil, 0, err
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	return out.Bytes(), p.Off, nil
}

// hasFDT reports whether b contains a plausible flattened device tree header.
func hasFDT(b []byte) bool {
	for i := 0; i+40 <= len(b); i++ {
		if binary.BigEndian.Uint32(b[i:]) != 0xd00dfeed {
			continue
		}
		size := binary.BigEndian.Uint32(b[i+4:])
		if size >= 40 && int(size) <= len(b)-i && binary.BigEndian.Uint32(b[i+20:]) >= 16 {
			return true
		}
	}
	return false
}
