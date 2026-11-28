package sh

import (
	"context"
	"image"
	"strings"
	"sync"
	"testing"
	"time"

	"godeep/kernel/gpu"
	"godeep/kernel/input"
)

// memDisplay is a gpu.Display in memory.
type memDisplay struct{ w, h int }

type memSurface struct {
	mu   sync.Mutex
	w, h int
	pix  []byte
}

func (d memDisplay) Size() image.Point { return image.Pt(d.w, d.h) }
func (d memDisplay) NewSurface(_ context.Context, w, h int) (gpu.Surface, error) {
	return &memSurface{w: w, h: h, pix: make([]byte, 4*w*h)}, nil
}
func (memDisplay) Show(context.Context, gpu.Surface) error { return nil }

func (s *memSurface) Bounds() image.Rectangle                        { return image.Rect(0, 0, s.w, s.h) }
func (s *memSurface) Pix() []byte                                    { return s.pix }
func (s *memSurface) Stride() int                                    { return 4 * s.w }
func (s *memSurface) Present(context.Context, image.Rectangle) error { return nil }

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (dk *desktop) list() []*terminal {
	dk.mu.Lock()
	defer dk.mu.Unlock()
	var out []*terminal
	for _, t := range dk.terms {
		out = append(out, t)
	}
	return out
}

func TestDesktop(t *testing.T) {
	s := newShell(t)
	keys := make(chan byte, 64)
	got := make(chan *desktop, 1)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	go func() {
		done <- runDesktop(ctx, s.env, s.sh, memDisplay{1280, 800}, keys, 2, func(dk *desktop) { got <- dk })
	}()
	dk := <-got
	waitUntil(t, "two terminals with prompts", func() bool {
		l := dk.list()
		if len(l) != 2 {
			return false
		}
		for _, tm := range l {
			if !strings.Contains(tm.screen.Text(), "godeep$") {
				return false
			}
		}
		return true
	})

	// typed bytes go to the focused terminal (the last opened)
	focused := dk.focused(ctx)
	for _, b := range []byte("echo hello from the desktop\r") {
		keys <- b
	}
	waitUntil(t, "echo output", func() bool {
		return strings.Contains(focused.screen.Text(), "\nhello from the desktop")
	})
	for _, tm := range dk.list() {
		if tm != focused && strings.Contains(tm.screen.Text(), "hello") {
			t.Fatal("the unfocused terminal got the keys")
		}
	}

	// [x] on the focused window kills its shell: process and window go
	cb, err := focused.win.CloseButton(ctx)
	if err != nil || cb.Empty() {
		t.Fatalf("close button %v %v", cb, err)
	}
	click := func(p image.Point) {
		dk.m.Input(input.Event{Kind: input.Absolute, X: int32(p.X * input.AbsMax / 1279), Y: int32(p.Y * input.AbsMax / 799)})
		dk.m.Input(input.Event{Kind: input.Button, Code: 0, Down: true})
		dk.m.Input(input.Event{Kind: input.Button, Code: 0, Down: false})
	}
	click(cb.Min.Add(cb.Size().Div(2)))
	waitUntil(t, "closed terminal gone", func() bool { return len(dk.list()) == 1 })
	select {
	case <-focused.p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("terminal process still running")
	}
	select {
	case <-focused.sh.Done():
	default:
		t.Fatal("its shell still running")
	}

	// a click on the empty desktop opens one; Super+W closes the focused
	click(image.Pt(1270, 790))
	waitUntil(t, "a new terminal", func() bool { return len(dk.list()) == 2 })
	dk.m.Input(input.Event{Kind: input.KeyDown, Code: 0x1a, Mods: input.ModLGUI})
	waitUntil(t, "Super+W closed one", func() bool { return len(dk.list()) == 1 })

	// typing exit in a terminal closes it too
	last := dk.focused(ctx)
	if last == nil {
		t.Fatal("no focused terminal")
	}
	for _, b := range []byte("exit\r") {
		keys <- b
	}
	waitUntil(t, "exit closed it", func() bool { return len(dk.list()) == 0 })

	// Super+Q leaves
	dk.m.Input(input.Event{Kind: input.KeyDown, Code: 0x28, Mods: input.ModLGUI}) // one more, to be killed on the way out
	waitUntil(t, "Super+Return opened one", func() bool { return len(dk.list()) == 1 })
	survivor := dk.list()[0]
	dk.m.Input(input.Event{Kind: input.KeyDown, Code: 0x14, Mods: input.ModLGUI})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Super+Q didn't leave")
	}
	select {
	case <-survivor.p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a terminal outlived the desktop")
	}
}
