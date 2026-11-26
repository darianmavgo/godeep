package block

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"unicode/utf16"
)

// Partition is a slice of a device; it is itself a Device.
type Partition struct {
	Index int    // 1-based, as in "nvme0p1"
	Name  string // GPT partition name ("" for MBR)
	Type  string // e.g. "EFI system", "Linux data", "MBR 0x83"
	Start uint64 // first block on the parent
	Count uint64 // blocks

	parent Device
}

func (p *Partition) BlockSize() int { return p.parent.BlockSize() }
func (p *Partition) Blocks() uint64 { return p.Count }

func (p *Partition) ReadBlocks(ctx context.Context, lba uint64, b []byte) error {
	if err := Check(p, lba, b); err != nil {
		return err
	}
	return p.parent.ReadBlocks(ctx, p.Start+lba, b)
}

// ErrNoTable means the device has neither a GPT nor an MBR.
var ErrNoTable = errors.New("block: no partition table")

// Partitions reads d's partition table: GPT (validated by its CRCs), else
// MBR. scheme is "gpt" or "mbr".
func Partitions(ctx context.Context, d Device) (parts []*Partition, scheme string, err error) {
	bs := d.BlockSize()
	if d.Blocks() < 2 {
		return nil, "", ErrNoTable
	}
	b := make([]byte, 2*bs)
	if err := d.ReadBlocks(ctx, 0, b); err != nil {
		return nil, "", err
	}
	mbr, hdr := b[:bs], b[bs:]
	if string(hdr[:8]) == "EFI PART" {
		p, err := gpt(ctx, d, hdr)
		return p, "gpt", err
	}
	if bs < 512 || mbr[510] != 0x55 || mbr[511] != 0xaa {
		return nil, "", ErrNoTable
	}
	for i := 0; i < 4; i++ {
		e := mbr[446+16*i:]
		typ := e[4]
		start, count := uint64(binary.LittleEndian.Uint32(e[8:])), uint64(binary.LittleEndian.Uint32(e[12:]))
		if typ == 0 || count == 0 {
			continue
		}
		if typ == 0xee {
			return nil, "", errors.New("block: protective MBR without a valid GPT")
		}
		if start+count > d.Blocks() {
			return nil, "", fmt.Errorf("block: MBR partition %d runs past the end of the disk", i+1)
		}
		parts = append(parts, &Partition{Index: i + 1, Type: fmt.Sprintf("MBR %#02x", typ), Start: start, Count: count, parent: d})
	}
	return parts, "mbr", nil
}

func gpt(ctx context.Context, d Device, hdr []byte) ([]*Partition, error) {
	le := binary.LittleEndian
	hsize := le.Uint32(hdr[12:])
	if hsize < 92 || int(hsize) > len(hdr) {
		return nil, fmt.Errorf("block: GPT header size %d", hsize)
	}
	h := append([]byte{}, hdr[:hsize]...)
	want := le.Uint32(h[16:])
	le.PutUint32(h[16:], 0)
	if crc32.ChecksumIEEE(h) != want {
		return nil, errors.New("block: GPT header checksum mismatch")
	}
	entLBA, num, esize := le.Uint64(hdr[72:]), le.Uint32(hdr[80:]), le.Uint32(hdr[84:])
	if esize < 128 || num > 1024 {
		return nil, fmt.Errorf("block: GPT with %d entries of %d bytes", num, esize)
	}
	bs := uint64(d.BlockSize())
	n := (uint64(num)*uint64(esize) + bs - 1) / bs
	ents := make([]byte, n*bs)
	if err := d.ReadBlocks(ctx, entLBA, ents); err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(ents[:num*esize]) != le.Uint32(hdr[88:]) {
		return nil, errors.New("block: GPT entry array checksum mismatch")
	}
	var parts []*Partition
	for i := uint32(0); i < num; i++ {
		e := ents[i*esize:]
		typ := guid(e[:16])
		if typ == "00000000-0000-0000-0000-000000000000" {
			continue
		}
		first, last := le.Uint64(e[32:]), le.Uint64(e[40:])
		if last < first || last >= d.Blocks() {
			return nil, fmt.Errorf("block: GPT partition %d has blocks %d..%d", i+1, first, last)
		}
		u := make([]uint16, 36)
		for j := range u {
			u[j] = le.Uint16(e[56+2*j:])
		}
		end := 0
		for end < len(u) && u[end] != 0 {
			end++
		}
		name := typeNames[typ]
		if name == "" {
			name = typ
		}
		parts = append(parts, &Partition{Index: int(i) + 1, Name: string(utf16.Decode(u[:end])), Type: name,
			Start: first, Count: last - first + 1, parent: d})
	}
	return parts, nil
}

// guid formats a GPT GUID (first three fields little-endian).
func guid(b []byte) string {
	le := binary.LittleEndian
	return fmt.Sprintf("%08X-%04X-%04X-%X-%X", le.Uint32(b), le.Uint16(b[4:]), le.Uint16(b[6:]), b[8:10], b[10:16])
}

// GUID parses "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX" into GPT byte order.
func GUID(s string) ([16]byte, error) {
	var g [16]byte
	var a uint32
	var b, c uint16
	var d [8]byte
	if _, err := fmt.Sscanf(s, "%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X", &a, &b, &c,
		&d[0], &d[1], &d[2], &d[3], &d[4], &d[5], &d[6], &d[7]); err != nil {
		return g, fmt.Errorf("block: bad GUID %q: %v", s, err)
	}
	le := binary.LittleEndian
	le.PutUint32(g[:], a)
	le.PutUint16(g[4:], b)
	le.PutUint16(g[6:], c)
	copy(g[8:], d[:])
	return g, nil
}

// Well-known GPT partition types.
const (
	TypeEFI       = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	TypeLinux     = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
	TypeBasicData = "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7"
	TypeAPFS      = "7C3457EF-0000-11AA-AA11-00306543ECAC"
)

var typeNames = map[string]string{
	TypeEFI:                                "EFI system",
	TypeLinux:                              "Linux data",
	TypeBasicData:                          "basic data",
	TypeAPFS:                               "APFS",
	"69646961-6700-11AA-AA11-00306543ECAC": "Apple boot (iBoot)",
	"52637672-7900-11AA-AA11-00306543ECAC": "Apple recovery",
}
