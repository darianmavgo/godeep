package input

import (
	"sort"
	"strings"
	"sync/atomic"
)

// Layout maps what a US keyboard would type to what this layout types on
// the same key: Dvorak and Colemak move the letters and punctuation, UK
// swaps " and @ and adds the ISO keys. Ctrl and Caps Lock apply after the
// remap, so Ctrl-C is the key that types c in the layout.
type Layout struct {
	Name  string
	remap map[byte]byte
	iso   map[byte][2]byte // extra usages (0x32 non-US #, 0x64 non-US \): plain, shifted
}

func newLayout(name, us, to string, iso map[byte][2]byte) *Layout {
	l := &Layout{Name: name, remap: map[byte]byte{}, iso: iso}
	for i := 0; i < len(us); i++ {
		l.remap[us[i]] = to[i]
	}
	return l
}

// Layouts are the built-in layouts by name.
var Layouts = map[string]*Layout{
	"us": {Name: "us"},
	"uk": newLayout("uk", `"@`, `@"`, map[byte][2]byte{0x32: {'#', '~'}, 0x64: {'\\', '|'}}),
	"dvorak": newLayout("dvorak",
		`qwertyuiop[]asdfghjkl;'zxcvbnm,./-=QWERTYUIOP{}ASDFGHJKL:"ZXCVBNM<>?_+`,
		`',.pyfgcrl/=aoeuidhtns-;qjkxbmwvz[]"<>PYFGCRL?+AOEUIDHTNS_:QJKXBMWVZ{}`, nil),
	"colemak": newLayout("colemak",
		`ertyuiopsdfgjkl;nERTYUIOPSDFGJKL:N`,
		`fpgjluy;rstdneiokFPGJLUY:RSTDNEIOK`, nil),
}

// LayoutNames lists the built-in layouts.
func LayoutNames() []string {
	var n []string
	for k := range Layouts {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// LayoutSetter is a keyboard driver's service that can switch layouts.
type LayoutSetter interface{ SetLayout(*Layout) }

// layoutRef is a Keyboard's current layout, set from another goroutine.
type layoutRef struct{ p atomic.Pointer[Layout] }

func (r *layoutRef) get() *Layout {
	if l := r.p.Load(); l != nil {
		return l
	}
	return Layouts["us"]
}

// SetLayout switches the keyboard's layout (safe from any goroutine).
func (d *Keyboard) SetLayout(l *Layout) { d.layout.p.Store(l) }

// Layout is the keyboard's current layout.
func (d *Keyboard) Layout() *Layout { return d.layout.get() }

// translate is Translate in a layout.
func (l *Layout) translate(k, mods byte, caps bool) []byte {
	shift := mods&(ModLShift|ModRShift) != 0
	ctrl := mods&(ModLCtrl|ModRCtrl) != 0
	if v, ok := l.iso[k]; ok {
		if shift {
			return []byte{v[1]}
		}
		return []byte{v[0]}
	}
	if (k < 0x04 || k > 0x38) || k == 0x28 || k == 0x29 || k == 0x2a || k == 0x2b || k == 0x2c {
		return Translate(k, mods, caps) // arrows, enter, escape, backspace, tab, space
	}
	us := Translate(k, mods&^(ModLCtrl|ModRCtrl), false) // the US character, shift applied
	if len(us) != 1 {
		return us
	}
	c := us[0]
	if r, ok := l.remap[c]; ok {
		c = r
	}
	isLetter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	if ctrl && isLetter {
		return []byte{c & 0x1f}
	}
	if caps && isLetter {
		if c >= 'a' {
			c -= 'a' - 'A'
		} else {
			c += 'a' - 'A'
		}
	}
	return []byte{c}
}

// String describes a layout for listings.
func (l *Layout) String() string { return strings.ToLower(l.Name) }
