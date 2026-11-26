package block

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"unicode/utf16"
)

// PartSpec describes one partition for MakeGPT.
type PartSpec struct {
	Name string
	Type string // GUID, e.g. TypeLinux
	Data []byte // contents (size rounded up to whole blocks)
}

// MakeGPT builds a disk image of the given size (bytes, a multiple of bs)
// with a protective MBR, primary and backup GPT, and the partitions packed
// from 1 MiB on, each 1 MiB aligned. Used by tools/mkdisk and tests; the
// kernel itself only reads partition tables.
func MakeGPT(size int64, bs int, parts []PartSpec) ([]byte, error) {
	const entries, esize = 128, 128
	le := binary.LittleEndian
	img := make([]byte, size)
	nblk := uint64(size) / uint64(bs)
	entBlocks := uint64(entries*esize) / uint64(bs)
	firstUsable, lastUsable := 2+entBlocks, nblk-2-entBlocks
	align := uint64(1<<20) / uint64(bs)

	ents := make([]byte, entries*esize)
	next := align
	for i, p := range parts {
		typ, err := GUID(p.Type)
		if err != nil {
			return nil, err
		}
		n := (uint64(len(p.Data)) + uint64(bs) - 1) / uint64(bs)
		if n == 0 || next+n-1 > lastUsable {
			return nil, errors.New("block: partitions don't fit on the disk")
		}
		copy(img[next*uint64(bs):], p.Data)
		e := ents[i*esize:]
		copy(e, typ[:])
		uniq := typ // unique GUID: derived, stable across runs
		uniq[15] ^= byte(i + 1)
		uniq[0] ^= 0x5a
		copy(e[16:], uniq[:])
		le.PutUint64(e[32:], next)
		le.PutUint64(e[40:], next+n-1)
		for j, c := range utf16.Encode([]rune(p.Name)) {
			if j < 36 {
				le.PutUint16(e[56+2*j:], c)
			}
		}
		next = (next + n + align - 1) / align * align
	}

	// protective MBR
	m := img[:512]
	m[446+4] = 0xee
	le.PutUint32(m[446+8:], 1)
	cnt := nblk - 1
	if cnt > 0xffffffff {
		cnt = 0xffffffff
	}
	le.PutUint32(m[446+12:], uint32(cnt))
	m[510], m[511] = 0x55, 0xaa

	header := func(my, alt, entLBA uint64) []byte {
		h := make([]byte, 92)
		copy(h, "EFI PART")
		le.PutUint32(h[8:], 0x00010000)
		le.PutUint32(h[12:], 92)
		le.PutUint64(h[24:], my)
		le.PutUint64(h[32:], alt)
		le.PutUint64(h[40:], firstUsable)
		le.PutUint64(h[48:], lastUsable)
		copy(h[56:], "godeep-disk-guid")
		le.PutUint64(h[72:], entLBA)
		le.PutUint32(h[80:], entries)
		le.PutUint32(h[84:], esize)
		le.PutUint32(h[88:], crc32.ChecksumIEEE(ents))
		le.PutUint32(h[16:], crc32.ChecksumIEEE(h))
		return h
	}
	copy(img[uint64(bs):], header(1, nblk-1, 2))
	copy(img[2*uint64(bs):], ents)
	copy(img[(nblk-1-entBlocks)*uint64(bs):], ents)
	copy(img[(nblk-1)*uint64(bs):], header(nblk-1, 1, nblk-1-entBlocks))
	return img, nil
}
