// Package font draws anti-aliased TrueType text into godeep surfaces
// (B, G, R, X bytes, gpu.Surface's layout). The fonts are Go's own (Go
// Mono, Go Regular; BSD licence, golang.org/x/image/font/gofont), rasterized
// by golang.org/x/image/font/opentype: pure Go, nothing to load at run time.
//
// A Face caches each glyph's coverage mask the first time it's drawn, so
// a terminal redrawing a screen of text only blends masks. Faces are safe
// for concurrent use.
package font

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"sync"

	xfont "golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Style selects a face's variant.
type Style uint8

const (
	Regular Style = iota
	Bold
)

// Face is one family at one size, Regular and Bold.
type Face struct {
	Advance int // monospace cell width (the width of 'M' for proportional faces)
	Ascent  int // baseline below the top of a line
	Height  int // line height

	mu     sync.Mutex
	faces  [2]xfont.Face // opentype faces aren't safe for concurrent use
	glyphs map[glyphKey]*glyph
}

type glyphKey struct {
	r     rune
	style Style
}

// glyph is a coverage mask and where it sits relative to the pen
// (x at the origin, y at the baseline).
type glyph struct {
	mask    *image.Alpha
	off     image.Point
	advance int
}

var (
	parseOnce              sync.Once
	mono, monoBold, uiFont *opentype.Font
	parseErr               error
)

func parse() {
	parseOnce.Do(func() {
		for _, f := range []struct {
			dst **opentype.Font
			ttf []byte
		}{{&mono, gomono.TTF}, {&monoBold, gomonobold.TTF}, {&uiFont, goregular.TTF}} {
			if *f.dst, parseErr = opentype.Parse(f.ttf); parseErr != nil {
				return
			}
		}
	})
}

// Mono returns Go Mono (Regular and Bold) at size pixels: terminal text.
func Mono(size float64) (*Face, error) {
	parse()
	if parseErr != nil {
		return nil, parseErr
	}
	return newFace(size, mono, monoBold)
}

// UI returns Go Regular at size pixels (Bold falls back to Regular): title
// bars and labels.
func UI(size float64) (*Face, error) {
	parse()
	if parseErr != nil {
		return nil, parseErr
	}
	return newFace(size, uiFont, uiFont)
}

func newFace(size float64, regular, bold *opentype.Font) (*Face, error) {
	f := &Face{glyphs: map[glyphKey]*glyph{}}
	for i, src := range []*opentype.Font{regular, bold} {
		face, err := opentype.NewFace(src, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: xfont.HintingFull})
		if err != nil {
			return nil, err
		}
		f.faces[i] = face
	}
	m := f.faces[0].Metrics()
	f.Ascent = m.Ascent.Ceil()
	f.Height = int(math.Ceil(size * 1.25)) // a little leading over ascent + descent
	f.Height = max(f.Height, (m.Ascent + m.Descent).Ceil())
	if adv, ok := f.faces[0].GlyphAdvance('M'); ok {
		f.Advance = adv.Round()
	}
	return f, nil
}

// get returns the cached glyph for r, rasterizing it on first use.
func (f *Face) get(r rune, s Style) *glyph {
	k := glyphKey{r, s}
	f.mu.Lock()
	defer f.mu.Unlock()
	if g, ok := f.glyphs[k]; ok {
		return g
	}
	face := f.faces[s&1]
	dr, mask, mp, adv, ok := face.Glyph(fixed.P(0, 0), r)
	if !ok {
		dr, mask, mp, adv, ok = face.Glyph(fixed.P(0, 0), 0xfffd) // replacement character
	}
	g := &glyph{advance: adv.Round()}
	if ok && !dr.Empty() {
		a := image.NewAlpha(image.Rect(0, 0, dr.Dx(), dr.Dy()))
		draw.Draw(a, a.Bounds(), mask, mp, draw.Src)
		g.mask, g.off = a, dr.Min
	}
	f.glyphs[k] = g
	return g
}

// Glyph draws r with its pen at x and the line's top at y (the baseline is
// y + Ascent), blending fg over what's there, clipped to clip and to the
// pixel buffer (stride bytes per row, B, G, R, X). It returns the advance.
func (f *Face) Glyph(pix []byte, stride int, clip image.Rectangle, x, y int, r rune, s Style, fg color.RGBA) int {
	g := f.get(r, s)
	if g.mask == nil {
		return g.advance
	}
	at := image.Pt(x, y+f.Ascent).Add(g.off)
	dst := g.mask.Bounds().Add(at).Intersect(clip)
	if stride > 0 {
		dst = dst.Intersect(image.Rect(0, 0, stride/4, len(pix)/stride))
	}
	for py := dst.Min.Y; py < dst.Max.Y; py++ {
		mrow := g.mask.Pix[(py-at.Y)*g.mask.Stride:]
		prow := pix[py*stride:]
		for px := dst.Min.X; px < dst.Max.X; px++ {
			a := uint32(mrow[px-at.X])
			if a == 0 {
				continue
			}
			p := prow[4*px : 4*px+4]
			if a == 0xff {
				p[0], p[1], p[2] = fg.B, fg.G, fg.R
				continue
			}
			p[0] = blend(p[0], fg.B, a)
			p[1] = blend(p[1], fg.G, a)
			p[2] = blend(p[2], fg.R, a)
		}
	}
	return g.advance
}

func blend(dst, src uint8, a uint32) uint8 {
	return uint8((uint32(src)*a + uint32(dst)*(255-a) + 127) / 255)
}

// String draws s from x with the line's top at y; it returns the x after
// the last glyph.
func (f *Face) String(pix []byte, stride int, clip image.Rectangle, x, y int, s string, st Style, fg color.RGBA) int {
	for _, r := range s {
		x += f.Glyph(pix, stride, clip, x, y, r, st, fg)
	}
	return x
}

// Width is how wide s draws.
func (f *Face) Width(s string) int {
	w := 0
	for _, r := range s {
		w += f.get(r, Regular).advance
	}
	return w
}
