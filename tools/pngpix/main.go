// Command pngpix prints the colors of pixels in a PNG, for checks on
// QEMU screendumps: pngpix FILE X,Y [X,Y...] prints "#rrggbb" per point.
// With -bars N it prints the color at the middle of each of N equal
// vertical bars, a quarter of the way down (gpu.TestPattern's bars).
package main

import (
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"strconv"
	"strings"
)

func main() {
	bars := flag.Int("bars", 0, "sample N vertical bars")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: pngpix [-bars N] FILE [X,Y...]")
		os.Exit(2)
	}
	f, err := os.Open(flag.Arg(0))
	if err != nil {
		fail(err)
	}
	img, err := png.Decode(f)
	if err != nil {
		fail(err)
	}
	b := img.Bounds()
	var pts []image.Point
	for i := 0; i < *bars; i++ {
		pts = append(pts, image.Pt(b.Min.X+(2*i+1)*b.Dx()/(2**bars), b.Min.Y+b.Dy()/4))
	}
	for _, a := range flag.Args()[1:] {
		xs, ys, ok := strings.Cut(a, ",")
		x, err1 := strconv.Atoi(xs)
		y, err2 := strconv.Atoi(ys)
		if !ok || err1 != nil || err2 != nil {
			fail(fmt.Errorf("bad point %q", a))
		}
		pts = append(pts, image.Pt(x, y))
	}
	fmt.Printf("%dx%d", b.Dx(), b.Dy())
	for _, p := range pts {
		r, g, bl, _ := img.At(p.X, p.Y).RGBA()
		fmt.Printf(" #%02x%02x%02x", r>>8, g>>8, bl>>8)
	}
	fmt.Println()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "pngpix:", err)
	os.Exit(1)
}
