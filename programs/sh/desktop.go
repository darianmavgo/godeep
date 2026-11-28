package sh

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"strconv"
	"sync"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/font"
	"godeep/kernel/gpu"
	"godeep/kernel/input"
	"godeep/kernel/proc"
	"godeep/kernel/tty"
	"godeep/kernel/vt"
	"godeep/kernel/wm"
)

// desktop: terminal windows on the screen. The desktop (this builtin's
// goroutines) owns the compositor and the keyboard; each terminal is a
// process of its own ("termN") with a window, a vt screen, a tty and a
// child sh, like an SSH session. Closing a window ([x], Super+W) kills that
// terminal's shell, which ends its process and so its window; exiting the
// shell does the same.
//
// Keys: typed bytes come from this shell's terminal (TTY.Raw: the console
// keyboard, the Mac's internal one, or the SSH/serial line this was started
// from) and go to the focused window, ^C included. Super combinations are
// the desktop's (they type nothing): Return opens a terminal, W closes the
// focused one, Q leaves. A click on the empty desktop opens a terminal.

func init() {
	builtins["desktop"] = builtin{"desktop [-n N] [-t SECONDS]: terminal windows on the screen, each with its own sh; [x] or exit closes one, a click on the desktop or Super+Return opens one, Super+Q leaves", desktopCmd}
}

// Tokyo Night, for the terminals.
var (
	termBG     = [3]uint8{0x1a, 0x1b, 0x26}
	termFG     = [3]uint8{0xc0, 0xca, 0xf5}
	termCursor = [3]uint8{0xc0, 0xca, 0xf5}
	termANSI   = [16][3]uint8{
		{0x15, 0x16, 0x1e}, {0xf7, 0x76, 0x8e}, {0x9e, 0xce, 0x6a}, {0xe0, 0xaf, 0x68},
		{0x7a, 0xa2, 0xf7}, {0xbb, 0x9a, 0xf7}, {0x7d, 0xcf, 0xff}, {0xa9, 0xb1, 0xd6},
		{0x41, 0x48, 0x68}, {0xf7, 0x76, 0x8e}, {0x9e, 0xce, 0x6a}, {0xe0, 0xaf, 0x68},
		{0x7a, 0xa2, 0xf7}, {0xbb, 0x9a, 0xf7}, {0x7d, 0xcf, 0xff}, {0xc0, 0xca, 0xf5},
	}
)

// findDisplay is the first ready gpu.Display, preferring one that isn't
// the framebuffer console (virtio-gpu on QEMU) over the console's.
func findDisplay(env Env) (gpu.Display, string, error) {
	if env.Dev == nil {
		return nil, "", errors.New("no devices")
	}
	var fallback gpu.Display
	var fallbackName string
	for _, s := range env.Dev.List() {
		g, ok := s.Service.(gpu.Display)
		if !ok || s.State != dev.Ready {
			continue
		}
		if _, console := s.Service.(interface{ ConsoleDisplay() }); console {
			if fallback == nil {
				fallback, fallbackName = g, s.Name
			}
			continue
		}
		return g, s.Name, nil
	}
	if fallback != nil {
		return fallback, fallbackName, nil
	}
	return nil, "", errors.New("no display (QEMU: -device virtio-gpu-device or ramfb; Parallels/Mac: the framebuffer console)")
}

type desktop struct {
	env    Env
	parent *proc.Proc
	m      *wm.Manager
	theme  wm.Theme
	mono   *font.Face
	scale  int

	mu    sync.Mutex
	terms map[*wm.Window]*terminal
	n     int // terminals opened so far (names)
}

type terminal struct {
	p      *proc.Proc
	sh     *proc.Proc
	in     chan byte
	win    *wm.Window
	screen *vt.Term
}

func desktopCmd(ctx context.Context, env Env, p *proc.Proc, args []string) error {
	n, secs := 1, 0.0
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return errors.New("usage: desktop [-n N] [-t SECONDS]")
		}
		var err error
		switch args[i] {
		case "-n":
			n, err = strconv.Atoi(args[i+1])
			if err != nil || n < 0 || n > 8 {
				return fmt.Errorf("bad -n %q (0-8)", args[i+1])
			}
		case "-t":
			secs, err = strconv.ParseFloat(args[i+1], 64)
			if err != nil || secs <= 0 {
				return fmt.Errorf("bad -t %q", args[i+1])
			}
		default:
			return errors.New("usage: desktop [-n N] [-t SECONDS]")
		}
		i++
	}
	d, name, err := findDisplay(env)
	if err != nil {
		return err
	}
	var keys <-chan byte
	if env.TTY != nil && env.In == nil {
		raw, release, err := env.TTY.Raw()
		if err != nil {
			return err
		}
		defer release()
		keys = raw
	}
	if secs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(secs*float64(time.Second)))
		defer cancel()
	}
	if env.Out != nil || env.TTY == nil {
		fmt.Fprintf(env.out(), "desktop: on %s %dx%d\n", name, d.Size().X, d.Size().Y)
	}
	return runDesktop(ctx, env, p, d, keys, n, nil)
}

// runDesktop runs the desktop on d with keys as the keyboard and n
// terminals to start with, until Super+Q or ctx ends. started (tests) gets
// the desktop once its compositor runs.
func runDesktop(ctx context.Context, env Env, p *proc.Proc, d gpu.Display, keys <-chan byte, n int, started func(*desktop)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	scale := max(1, d.Size().X/1600)
	theme, err := wm.Night(scale)
	if err != nil {
		return err
	}
	theme.Hint = "click here or Super+Return: new terminal    [x] or exit: close it    Super+Q: leave the desktop"
	mono, err := font.Mono(float64(15 * scale))
	if err != nil {
		return err
	}
	dk := &desktop{env: env, parent: p, theme: theme, mono: mono, scale: scale, terms: map[*wm.Window]*terminal{}}
	actions := make(chan byte, 16) // 'n' new terminal, 'w' close focused, 'q' leave
	act := func(a byte) {
		select {
		case actions <- a:
		default:
		}
	}
	dk.m = wm.NewWith(d, wm.Options{Theme: theme,
		Keys: func(e input.Event) bool {
			if e.Mods&(input.ModLGUI|input.ModRGUI) == 0 {
				return false
			}
			if e.Kind == input.KeyDown {
				switch e.Code {
				case 0x28: // Return
					act('n')
				case 0x1a: // W
					act('w')
				case 0x14: // Q
					act('q')
				}
			}
			return true // Super combinations are the desktop's
		},
		Desktop: func(image.Point) { act('n') },
	})
	ran := make(chan error, 1)
	if !p.Go(func(context.Context) { ran <- dk.m.Run(ctx) }) {
		return proc.ErrKilled
	}
	if env.Dev != nil {
		p.Go(func(context.Context) { wmInput(ctx, env.Dev, dk.m, p) })
	}
	if keys != nil {
		p.Go(func(context.Context) { dk.route(ctx, keys) })
	}
	for range n {
		dk.open(ctx)
	}
	if started != nil {
		started(dk)
	}
	for {
		select {
		case a := <-actions:
			switch a {
			case 'n':
				dk.open(ctx)
			case 'w':
				if t := dk.focused(ctx); t != nil {
					t.sh.Kill()
				}
			case 'q':
				return nil
			}
		case <-ctx.Done():
			return nil
		case err := <-ran:
			return err
		}
	}
}

// route sends typed bytes to the focused terminal.
func (dk *desktop) route(ctx context.Context, keys <-chan byte) {
	for {
		var b byte
		select {
		case b = <-keys:
		case <-ctx.Done():
			return
		}
		t := dk.focused(ctx)
		if t == nil {
			continue
		}
		select {
		case t.in <- b:
		case <-t.p.Done():
		case <-ctx.Done():
			return
		}
	}
}

func (dk *desktop) focused(ctx context.Context) *terminal {
	w := dk.m.Focused(ctx)
	dk.mu.Lock()
	defer dk.mu.Unlock()
	return dk.terms[w]
}

// open starts a terminal process.
func (dk *desktop) open(ctx context.Context) {
	dk.mu.Lock()
	dk.n++
	name := fmt.Sprintf("term%d", dk.n)
	dk.mu.Unlock()
	dk.env.Table.Spawn(ctx, dk.parent.PID, proc.Spec{Name: name, Main: func(ctx context.Context, p *proc.Proc) error {
		return dk.terminal(ctx, p, name)
	}})
}

// terminal is a terminal process: its window, screen, tty and shell.
func (dk *desktop) terminal(ctx context.Context, p *proc.Proc, name string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // closes the window
	sz := dk.m.Size()
	pad := 10 * dk.scale
	cw, ch := dk.mono.Advance, dk.mono.Height
	cols := min(100, max(20, (sz.X*3/5-2*pad)/cw))
	rows := min(32, max(5, (sz.Y*3/5-2*pad)/ch))
	w, h := cols*cw+2*pad, rows*ch+2*pad
	win, err := dk.m.NewWindow(ctx, name+" · sh", w, h)
	if err != nil {
		return err
	}
	screen := vt.New(cols, rows)
	t := &terminal{p: p, in: make(chan byte, 1024), win: win, screen: screen}
	tt := tty.NewFor(p, t.in, screen, true)
	tt.SetSize(cols, rows)
	env := dk.env
	env.TTY, env.Out, env.In = tt, nil, nil
	t.sh = dk.env.Table.Spawn(ctx, p.PID, proc.Spec{Name: "sh", Stdout: tt, Main: Main(env)})
	dk.mu.Lock()
	dk.terms[win] = t
	dk.mu.Unlock()
	defer func() {
		dk.mu.Lock()
		delete(dk.terms, win)
		dk.mu.Unlock()
	}()
	p.Go(func(context.Context) { dk.render(ctx, win, screen, pad, name) })
	select {
	case <-t.sh.Done():
	case <-win.CloseRequests():
		t.sh.Kill()
	case <-ctx.Done():
		t.sh.Kill()
	}
	<-t.sh.Done()
	return nil
}

// render draws the screen's changed rows into the window, at most every
// frame interval (output storms coalesce), and handles scrolling back.
func (dk *desktop) render(ctx context.Context, win *wm.Window, screen *vt.Term, pad int, name string) {
	const frame = 12 * time.Millisecond
	pix, stride := win.Pix(), win.Stride()
	bg := color.RGBA{termBG[0], termBG[1], termBG[2], 0xff}
	gpu.Fill(win, win.Bounds(), bg)
	win.Present(ctx, win.Bounds())
	ch := dk.mono.Height
	back := 0
	lastCY := -1
	title := ""
	for {
		var damage image.Rectangle
		screen.View(back, func(s vt.Screen) {
			for y := range s.Rows {
				if !s.Dirty[y] && y != lastCY && y != s.CursorY {
					continue
				}
				cursorX := -1
				if s.CursorOn && y == s.CursorY {
					cursorX = s.CursorX
				}
				dk.drawRow(pix, stride, pad, y, s.Rows[y], cursorX)
				damage = damage.Union(image.Rect(0, pad+y*ch, win.Bounds().Dx(), pad+(y+1)*ch))
			}
			lastCY = s.CursorY
			if !s.CursorOn {
				lastCY = -1
			}
		})
		if !damage.Empty() {
			if win.Present(ctx, damage) != nil {
				return
			}
		}
		if tl := screen.Title(); tl != title {
			title = tl
			label := name + " · sh"
			if tl != "" {
				label = name + " · " + tl
			}
			win.SetTitle(ctx, label)
		}
		select {
		case <-screen.Changed():
			if back != 0 { // new output returns to the live screen
				back = 0
				screen.Invalidate()
			}
		case e := <-win.Events():
			if e.Kind == input.Wheel && e.DY != 0 {
				nb := min(max(0, back+int(e.DY)*3), screen.Scrollback())
				if nb != back {
					back = nb
					screen.Invalidate()
				}
			}
			continue // no frame wait for input
		case <-ctx.Done():
			return
		}
		select {
		case <-time.After(frame):
		case <-ctx.Done():
			return
		}
	}
}

// drawRow paints one row of cells: backgrounds, glyphs, underline, and the
// cursor (a block, the cell inverted) at cursorX (-1: none).
func (dk *desktop) drawRow(pix []byte, stride, pad, y int, row []vt.Cell, cursorX int) {
	cw, ch := dk.mono.Advance, dk.mono.Height
	y0 := pad + y*ch
	width := stride / 4
	clip := image.Rect(0, 0, width, len(pix)/stride)
	// the padding beside the row
	fillRect(pix, stride, image.Rect(0, y0, pad, y0+ch), termBG)
	fillRect(pix, stride, image.Rect(pad+len(row)*cw, y0, width, y0+ch), termBG)
	for x, c := range row {
		fg := vt.Palette(c.FG, &termANSI, termFG)
		if c.Attr&vt.Bold != 0 && c.FG < 8 {
			fg = termANSI[c.FG+8]
		}
		bgc := vt.Palette(c.BG, &termANSI, termBG)
		if c.Attr&vt.Reverse != 0 {
			fg, bgc = bgc, fg
		}
		if x == cursorX {
			fg, bgc = termBG, termCursor
		}
		cell := image.Rect(pad+x*cw, y0, pad+(x+1)*cw, y0+ch)
		fillRect(pix, stride, cell, bgc)
		if c.R != ' ' && c.R != 0 {
			st := font.Regular
			if c.Attr&vt.Bold != 0 {
				st = font.Bold
			}
			dk.mono.Glyph(pix, stride, cell.Intersect(clip), cell.Min.X, y0, c.R, st, color.RGBA{fg[0], fg[1], fg[2], 0xff})
		}
		if c.Attr&vt.Underline != 0 {
			u := max(1, dk.scale)
			fillRect(pix, stride, image.Rect(cell.Min.X, y0+dk.mono.Ascent+2*u, cell.Max.X, y0+dk.mono.Ascent+3*u), fg)
		}
	}
}

// fillRect paints r (clipped to the buffer) without allocating.
func fillRect(pix []byte, stride int, r image.Rectangle, c [3]uint8) {
	r = r.Intersect(image.Rect(0, 0, stride/4, len(pix)/stride))
	for y := r.Min.Y; y < r.Max.Y; y++ {
		row := pix[y*stride+4*r.Min.X : y*stride+4*r.Max.X]
		for i := 0; i < len(row); i += 4 {
			row[i], row[i+1], row[i+2], row[i+3] = c[2], c[1], c[0], 0xff
		}
	}
}
