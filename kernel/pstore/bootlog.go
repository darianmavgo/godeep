package pstore

// The boot log is a breadcrumb trail of one boot's earliest steps, kept in
// RAM that survives a warm reset, for boards without a serial port: it is
// written from the first instruction of the entry code (one byte per step,
// in assembly) and then by Go code before the runtime exists (Hwinit0),
// long before the console or Ring exist. A board keeps two halves of
// BootLogSize bytes: this boot's and, copied there by the entry code, the
// previous boot's.
//
// Layout of a half:
//
//	0  magic "GDBOOTLG"
//	8  bytes appended (uint32, little endian; may exceed the capacity)
//	16 text
//
// Everything here is allocation-free and //go:nosplit (it runs before the
// Go runtime, and from assembly-adjacent code).
const (
	BootLogSize   = 0x1000
	bootLogHeader = 16
	bootLogMagic  = "GDBOOTLG"
	// BootLogMagic64 is the magic as a little-endian uint64 (for assembly).
	BootLogMagic64 = 0x474c544f4f424447
)

// BootLogReset starts an empty log in half.
//
//go:nosplit
func BootLogReset(half []byte) {
	if len(half) < BootLogSize {
		return
	}
	for i := 0; i < len(bootLogMagic); i++ {
		half[i] = bootLogMagic[i]
	}
	for i := 8; i < bootLogHeader; i++ {
		half[i] = 0
	}
}

//go:nosplit
func bootLogValid(half []byte) bool {
	if len(half) < BootLogSize {
		return false
	}
	for i := 0; i < len(bootLogMagic); i++ {
		if half[i] != bootLogMagic[i] {
			return false
		}
	}
	return true
}

//go:nosplit
func bootLogLen(half []byte) uint32 {
	return uint32(half[8]) | uint32(half[9])<<8 | uint32(half[10])<<16 | uint32(half[11])<<24
}

// BootLogAppend appends s to the log in half (nothing if half holds no
// log). Text past the capacity is counted but dropped. It returns the byte
// range [lo, hi) of half it changed, header included, so the caller can
// write it back to memory (cache maintenance).
//
//go:nosplit
func BootLogAppend(half []byte, s string) (lo, hi int) {
	if !bootLogValid(half) || len(s) == 0 {
		return 0, 0
	}
	n := bootLogLen(half)
	start := bootLogHeader + int(min(n, BootLogSize-bootLogHeader))
	at := start
	for i := 0; i < len(s) && at < BootLogSize; i++ {
		half[at] = s[i]
		at++
	}
	n += uint32(len(s))
	half[8], half[9], half[10], half[11] = byte(n), byte(n>>8), byte(n>>16), byte(n>>24)
	return 8, at
}

// BootLogText returns the text in half (a slice of it, not a copy), and
// whether half holds a log at all. truncated reports text dropped at the end.
func BootLogText(half []byte) (text []byte, truncated, ok bool) {
	if !bootLogValid(half) {
		return nil, false, false
	}
	n := bootLogLen(half)
	end := bootLogHeader + int(min(n, BootLogSize-bootLogHeader))
	return half[bootLogHeader:end], n > BootLogSize-bootLogHeader, true
}
