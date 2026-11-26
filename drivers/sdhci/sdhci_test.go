package sdhci

import "testing"

// put sets CSD/CID bits [hi:lo] in a long response (bits 127:8 shifted down by 8).
func put(v *[4]uint32, hi, lo int, x uint64) {
	for b := lo; b <= hi; b++ {
		i := b - 8
		if x>>(b-lo)&1 != 0 {
			v[i/32] |= 1 << (i % 32)
		}
	}
}

func TestCapacity(t *testing.T) {
	var v2 [4]uint32
	put(&v2, 127, 126, 1)
	put(&v2, 69, 48, 15159) // a 7.4 GB SDHC card
	if got, err := capacity(v2); err != nil || got != 15160*1024 {
		t.Fatalf("SDHC: %d %v", got, err)
	}
	var v1 [4]uint32
	put(&v1, 73, 62, 3965) // C_SIZE
	put(&v1, 49, 47, 7)    // C_SIZE_MULT: x512
	put(&v1, 83, 80, 9)    // READ_BL_LEN: 512
	if got, err := capacity(v1); err != nil || got != 3966*512 {
		t.Fatalf("SDSC: %d %v", got, err)
	}
	var cid [4]uint32
	put(&cid, 103, 64, uint64('Q')<<32|uint64('E')<<24|uint64('M')<<16|uint64('U')<<8|'!')
	if got := bits(cid, 103, 64); got != uint64('Q')<<32|uint64('E')<<24|uint64('M')<<16|uint64('U')<<8|'!' {
		t.Fatalf("product name bits %#x", got)
	}
}
