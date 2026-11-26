package simplefb

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"godeep/kernel/input"
)

func newTestConsole(t *testing.T) (*Console, *Framebuffer) {
	t.Helper()
	fb := &Framebuffer{Width: 7 * 40, Height: 13 * 10, Stride: 7 * 40 * 4, Format: X8R8G8B8}
	fb.Pix = make([]byte, fb.Stride*fb.Height)
	c, err := NewConsole(fb, 1)
	if err != nil {
		t.Fatal(err)
	}
	return c, fb
}

// screenText reads the visible rows back from the text model.
func screenText(c *Console) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for row := 0; row < c.rows; row++ {
		out = append(out, strings.TrimRight(string(c.lines[c.top()-c.view+row]), " "))
	}
	return out
}

// pixelsMatch checks the framebuffer against a fresh render of the model:
// scrolling by moving pixels must give the same picture as redrawing.
func pixelsMatch(t *testing.T, c *Console, fb *Framebuffer) {
	t.Helper()
	c.mu.Lock()
	c.hidePointer()
	got := append([]byte(nil), fb.Pix...)
	c.redraw()
	want := append([]byte(nil), fb.Pix...)
	c.showPointer()
	c.mu.Unlock()
	if string(got) != string(want) {
		t.Fatal("framebuffer differs from the text it shows")
	}
}

func TestScrollback(t *testing.T) {
	c, fb := newTestConsole(t)
	for i := 0; i < 30; i++ {
		fmt.Fprintf(c, "line %d\n", i)
	}
	if s := screenText(c); s[0] != "line 21" || s[8] != "line 29" {
		t.Fatalf("live screen: %q", s)
	}
	if v := c.ScrollView(3); v != 3 {
		t.Fatalf("view %d", v)
	}
	if s := screenText(c); s[0] != "line 18" {
		t.Fatalf("3 back: %q", s[0])
	}
	pixelsMatch(t, c, fb)
	if v := c.ScrollView(100); v != 21 {
		t.Fatalf("scroll past the start: view %d", v)
	}
	if s := screenText(c); s[0] != "line 0" {
		t.Fatalf("oldest: %q", s[0])
	}
	pixelsMatch(t, c, fb)
	c.ScrollView(-5)
	pixelsMatch(t, c, fb)

	// Output snaps back to the live screen.
	fmt.Fprint(c, "new")
	if s := screenText(c); s[9] != "new" || s[0] != "line 21" {
		t.Fatalf("after output: %q", s)
	}
	pixelsMatch(t, c, fb)
	if !strings.Contains(c.Status(), "21 lines of history") {
		t.Fatal(c.Status())
	}
}

func TestHistoryLimit(t *testing.T) {
	c, _ := newTestConsole(t)
	for i := 0; i < History+50; i++ {
		fmt.Fprintf(c, "%d\n", i)
	}
	c.mu.Lock()
	n := len(c.lines)
	c.mu.Unlock()
	if n != History+c.rows {
		t.Fatalf("%d lines kept", n)
	}
	c.ScrollView(History + 100)
	if s := screenText(c); s[0] != "41" {
		t.Fatalf("oldest kept line: %q", s[0])
	}
}

func TestPointerSavesPixels(t *testing.T) {
	c, fb := newTestConsole(t)
	fmt.Fprint(c, "hello\nworld")
	before := append([]byte(nil), fb.Pix...)
	if _, _, on := c.Pointer(); on {
		t.Fatal("pointer shown before any motion")
	}
	c.MovePointer(-1000, -1000) // to the corner, over "hello"
	if string(fb.Pix) == string(before) {
		t.Fatal("pointer not drawn")
	}
	if x, y, _ := c.Pointer(); x != 0 || y != 0 {
		t.Fatalf("not clamped: %d,%d", x, y)
	}
	c.MovePointer(5000, 5000) // to the far corner (clipped sprite)
	c.mu.Lock()
	c.hidePointer()
	restored := string(fb.Pix) == string(before)
	c.mu.Unlock()
	if !restored {
		t.Fatal("pixels under the pointer not restored")
	}
}

func TestSelectCopyPaste(t *testing.T) {
	c, fb := newTestConsole(t)
	fmt.Fprint(c, "first line\nsecond line\nthird")
	var mu sync.Mutex
	var pasted []byte
	m := &Mouse{C: c, Speed: 1, Paste: func(b []byte) { mu.Lock(); pasted = append(pasted, b...); mu.Unlock() }}

	// Move to column 6 of row 0 ("line"), drag to column 5 of row 1.
	m.HandleInput(input.Event{Kind: input.Motion, DX: -1000, DY: -1000})
	m.HandleInput(input.Event{Kind: input.Motion, DX: 6*7 + 1, DY: 1})
	m.HandleInput(input.Event{Kind: input.Button, Code: 0, Down: true})
	m.HandleInput(input.Event{Kind: input.Motion, DX: -1 * 7, DY: 13})
	m.HandleInput(input.Event{Kind: input.Button, Code: 0, Down: false})
	if got := m.Clip(); got != "line\nsecond" {
		t.Fatalf("copied %q", got)
	}
	if !strings.Contains(c.Status(), "selected") {
		t.Fatal("selection not shown")
	}
	pixelsMatch(t, c, fb)

	m.HandleInput(input.Event{Kind: input.Button, Code: 1, Down: true}) // right click
	if string(pasted) != "line\nsecond" {
		t.Fatalf("pasted %q", pasted)
	}

	// A plain click clears the selection and keeps the clipboard.
	m.HandleInput(input.Event{Kind: input.Button, Code: 0, Down: true})
	m.HandleInput(input.Event{Kind: input.Button, Code: 0, Down: false})
	if strings.Contains(c.Status(), "selected") || m.Clip() != "line\nsecond" {
		t.Fatalf("after click: %s, clip %q", c.Status(), m.Clip())
	}
	pixelsMatch(t, c, fb)
}

func TestWheelAndKeys(t *testing.T) {
	c, fb := newTestConsole(t)
	for i := 0; i < 40; i++ {
		fmt.Fprintf(c, "line %d\n", i)
	}
	m := &Mouse{C: c}
	m.HandleInput(input.Event{Kind: input.Wheel, DY: 2})
	if !strings.Contains(c.Status(), fmt.Sprintf("viewing %d back", 2*WheelLines)) {
		t.Fatal(c.Status())
	}
	pixelsMatch(t, c, fb)
	m.HandleInput(input.Event{Kind: input.KeyDown, Code: 4})
	if strings.Contains(c.Status(), "viewing") {
		t.Fatal("a key didn't return to the live screen")
	}
	pixelsMatch(t, c, fb)
}

func TestConcurrentOutputAndPointer(t *testing.T) {
	c, fb := newTestConsole(t)
	m := &Mouse{C: c, Speed: 1}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			fmt.Fprintf(c, "output %d\n", i)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			m.HandleInput(input.Event{Kind: input.Motion, DX: int32(i%7 - 3), DY: int32(i%5 - 2)})
			if i%50 == 0 {
				m.HandleInput(input.Event{Kind: input.Wheel, DY: 1})
			}
		}
	}()
	wg.Wait()
	c.ScrollView(-History)
	pixelsMatch(t, c, fb)
}
