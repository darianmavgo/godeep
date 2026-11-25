// Command mkimage turns a TamaGo arm64 ELF into a raw arm64 Linux "Image":
// the format QEMU (-kernel), m1n1 and U-Boot (booti) load. The loader places
// the image at <2MB-aligned RAM base> + text_offset and jumps to its first
// instruction with the device tree address in x0.
//
// The image covers every PT_LOAD segment (bss included, zero-filled) from the
// lowest segment address. Its first 64 bytes become the Image header; on
// TamaGo the lowest segment starts with the ELF header, which is never
// executed, so no code moves: code0 is a branch to the ELF entry point.
package main

import (
	"debug/elf"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sort"
)

func main() {
	ram := flag.Uint64("ram", 0x40000000, "RAM base the image is loaded relative to (2MB aligned)")
	out := flag.String("o", "", "output Image file")
	flag.Parse()
	if flag.NArg() != 1 || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: mkimage [-ram addr] -o out.img kernel.elf")
		os.Exit(2)
	}
	img, info, err := build(flag.Arg(0), *ram)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkimage:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, img, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "mkimage:", err)
		os.Exit(1)
	}
	fmt.Println(info)
}

func build(path string, ram uint64) ([]byte, string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	if f.Machine != elf.EM_AARCH64 {
		return nil, "", fmt.Errorf("%s is not an arm64 ELF", path)
	}
	var loads []*elf.Prog
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD {
			loads = append(loads, p)
		}
	}
	if len(loads) == 0 {
		return nil, "", fmt.Errorf("no PT_LOAD segments")
	}
	sort.Slice(loads, func(i, j int) bool { return loads[i].Vaddr < loads[j].Vaddr })
	base := loads[0].Vaddr
	end := uint64(0)
	for _, p := range loads {
		end = max(end, p.Vaddr+p.Memsz)
	}
	switch {
	case base < ram || base-ram >= 2<<20:
		return nil, "", fmt.Errorf("lowest segment %#x is not within 2MB above RAM base %#x", base, ram)
	case f.Entry < base+64 || f.Entry >= end:
		return nil, "", fmt.Errorf("entry %#x overlaps the 64-byte header at %#x", f.Entry, base)
	case loads[0].Off != 0 || loads[0].Filesz < 64:
		return nil, "", fmt.Errorf("first segment doesn't start with the ELF header; no room for the Image header")
	}
	size := (end - base + 0xfff) &^ 0xfff
	img := make([]byte, size)
	for _, p := range loads {
		if _, err := p.ReadAt(img[p.Vaddr-base:p.Vaddr-base+p.Filesz], 0); err != nil {
			return nil, "", err
		}
	}

	// arm64 Linux Image header (Documentation/arch/arm64/booting.rst).
	h := img[:64]
	clear(h)
	le := binary.LittleEndian
	le.PutUint32(h[0:], 0x14000000|uint32((f.Entry-base)/4)&0x03ffffff) // code0: b entry
	le.PutUint64(h[8:], base-ram)                                       // text_offset
	le.PutUint64(h[16:], size)                                          // image_size (incl. bss)
	le.PutUint64(h[24:], 1<<1)                                          // flags: LE, 4K pages, 2MB-aligned base
	le.PutUint32(h[56:], 0x644d5241)                                    // magic "ARM\x64"

	return img, fmt.Sprintf("Image: load at RAM+%#x (%#x), entry %#x, %d KB", base-ram, base, f.Entry, size/1024), nil
}
