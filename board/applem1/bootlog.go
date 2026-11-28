//go:build tamago && arm64 && applem1

package applem1

import (
	"unsafe"

	"godeep/kernel/pstore"
)

// The boot log: breadcrumbs of this boot's steps from the first instruction
// on, for the Mac, which has no serial port. entry_arm64.s writes one
// letter per step (E entry, R relocated, H EL2, V VHE, L EL1, F device tree,
// X vectors, M MMU on, G Go runtime next); from Hwinit0 on, blog adds
// text lines. Every write goes straight back to memory (dc civac), so a
// warm reset (the watchdog) keeps it at BOOTLOG_CUR, and the next boot's
// entry code moves it to BOOTLOG_PREV. Shown:
//
//   - live on the framebuffer under the stage squares, until the console
//     exists (then on the console, which takes the screen over);
//   - the previous boot's log: on the next boot's screen at stage 1 (in
//     grey, from the bottom up), on its console and in its kernel log
//     (dmesg, godeep/log.txt on GODEEPDATA), and as ram:/bootlog.prev.txt.

//go:nosplit
func bootlogHalf(base uint64) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(base))), pstore.BootLogSize)
}

// blog appends s to this boot's log, writes it back to memory and, until
// the console exists, draws it on the early screen. Boot core only.
//
//go:nosplit
func blog(s string) {
	lo, hi := pstore.BootLogAppend(bootlogHalf(BOOTLOG_CUR), s)
	for a := uint64(lo) &^ 63; a < uint64(hi); a += 64 {
		dcache_civac(BOOTLOG_CUR + a)
	}
	if console == nil {
		screenLog(s, false)
	} else {
		consoleLog(s)
	}
	if uartReady { // QEMU (tools/fakem1n1): mirrored to the terminal
		for i := 0; i < len(s); i++ {
			uartPut(s[i])
		}
	}
}

// consoleLog prints a boot log line on the console (after NewBoard made it).
//
//go:noinline
func consoleLog(s string) {
	console.Write([]byte(s))
}

// blogHex appends v in hex ("0x...", no leading zeros).
//
//go:nosplit
func blogHex(v uint64) {
	var b [18]byte
	i := len(b)
	for {
		i--
		d := byte(v & 0xf)
		if d < 10 {
			b[i] = '0' + d
		} else {
			b[i] = 'a' + d - 10
		}
		v >>= 4
		if v == 0 {
			break
		}
	}
	i--
	b[i] = 'x'
	i--
	b[i] = '0'
	blog(unsafe.String(&b[i], len(b)-i))
}

// blogDec appends v in decimal.
//
//go:nosplit
func blogDec(v uint64) {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = '0' + byte(v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	blog(unsafe.String(&b[i], len(b)-i))
}

// The early screen's text cursor (screenLog), and where this boot's text
// stops (the previous boot's log is drawn below).
var logX, logY, logBottom int

//go:nosplit
func logScale() int { return max(1, screen.Width/640) }

// screenLog draws s at the log cursor, below the stage squares, wrapping at
// the edge. prev draws in grey (the previous boot's log).
//
//go:nosplit
func screenLog(s string, prev bool) {
	if !screen.Valid() {
		return
	}
	sc := logScale()
	cw, lh := 6*sc, 10*sc
	left := screen.MarkSize() / 2
	if logY == 0 {
		_, y := screen.MarkOrigin(1)
		logX, logY = left, y+screen.MarkSize()+lh
	}
	c := uint32(0xffffff)
	if prev {
		c = 0x808080
	}
	c = screenColor(c)
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || logX+cw > screen.Width-left {
			logX, logY = left, logY+lh
			if s[i] == '\n' {
				continue
			}
		}
		if bottom := logBottom; logY+lh > screen.Height || (!prev && bottom > 0 && logY+lh > bottom) {
			return // full: the console will show the rest
		}
		// on its own background: the screen still shows whatever the
		// firmware drew (the Apple logo)
		screen.Rect(logX, logY-sc, logX+cw, logY+lh-sc, screenColor(0))
		screen.Text(logX, logY, sc, s[i:i+1], c)
		logX += cw
	}
}

//go:nosplit
func screenColor(rgb uint32) uint32 { return screen.Color(uint8(rgb>>16), uint8(rgb>>8), uint8(rgb)) }

// screenPrevLog draws the end of the previous boot's log, in grey, in the
// bottom quarter of the screen (stage 1): when this boot hangs as early as
// the last one, its screen shows both.
//
//go:nosplit
func screenPrevLog() {
	text, _, ok := pstore.BootLogText(bootlogHalf(BOOTLOG_PREV))
	if !ok || !screen.Valid() {
		return
	}
	sc := logScale()
	left := screen.MarkSize() / 2
	perLine := (screen.Width - 2*left) / (6 * sc)
	lines := (screen.Height / 4) / (10 * sc)
	if perLine <= 0 || lines <= 1 {
		return
	}
	// the last `lines-1` screen lines' worth of text (approximately: long
	// lines wrap), after a heading
	keep := (lines - 1) * perLine
	if len(text) > keep {
		text = text[len(text)-keep:]
	}
	x, y := logX, logY
	logX, logY = left, screen.Height-lines*10*sc
	logBottom = logY
	screenLog("previous boot's log:\n", true)
	screenLog(unsafe.String(unsafe.SliceData(text), len(text)), true)
	logX, logY = x, y
}

// prevBootLog returns the previous boot's log as text ("" if none), for
// NewBoard to print and keep.
func prevBootLog() string {
	text, trunc, ok := pstore.BootLogText(bootlogHalf(BOOTLOG_PREV))
	if !ok {
		return ""
	}
	s := string(text)
	if trunc {
		s += "\n(truncated)"
	}
	return s
}

// curBootLog returns this boot's log so far.
func curBootLog() string {
	text, _, _ := pstore.BootLogText(bootlogHalf(BOOTLOG_CUR))
	return string(text)
}
