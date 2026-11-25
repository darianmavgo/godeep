package irq

import (
	"sync"
	"testing"
	"time"
)

type fakeCtl struct {
	mu      sync.Mutex
	enabled map[int]bool
}

func (f *fakeCtl) Enable(id int)  { f.mu.Lock(); f.enabled[id] = true; f.mu.Unlock() }
func (f *fakeCtl) Disable(id int) { f.mu.Lock(); f.enabled[id] = false; f.mu.Unlock() }
func (f *fakeCtl) on(id int) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.enabled[id] }

func TestOneshotDispatch(t *testing.T) {
	ctl := &fakeCtl{enabled: map[int]bool{}}
	b := NewBridge(ctl)
	l, err := b.Request(33, "uart0")
	if err != nil {
		t.Fatal(err)
	}
	if !ctl.on(33) {
		t.Fatal("Request did not enable the line")
	}
	if _, err := b.Request(33, "other"); err == nil {
		t.Fatal("double claim allowed")
	}

	b.Dispatch(33)
	b.Dispatch(33) // coalesces, must not block
	if ctl.on(33) {
		t.Fatal("line still enabled while the driver hasn't serviced it")
	}
	select {
	case <-l.C:
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	select {
	case <-l.C:
		t.Fatal("events were not coalesced")
	default:
	}
	l.Done()
	if !ctl.on(33) {
		t.Fatal("Done did not re-enable")
	}

	b.Dispatch(99) // unowned
	st, spurious := b.Stats()
	if spurious != 1 || len(st) != 1 || st[0].Count != 2 {
		t.Fatalf("stats: %+v spurious=%d", st, spurious)
	}
	l.Release()
	if ctl.on(33) {
		t.Fatal("Release left the line enabled")
	}
}

func TestDriverLoop(t *testing.T) {
	ctl := &fakeCtl{enabled: map[int]bool{}}
	b := NewBridge(ctl)
	l, _ := b.Request(39, "pwrbtn")
	got := make(chan int)
	go func() { // a typical driver goroutine
		n := 0
		for range l.C {
			n++
			l.Done()
			if n == 3 {
				got <- n
				return
			}
		}
	}()
	for i := 0; i < 3; i++ {
		for !ctl.on(39) { // the "hardware" can only fire when enabled
			time.Sleep(time.Millisecond)
		}
		b.Dispatch(39)
	}
	if n := <-got; n != 3 {
		t.Fatal(n)
	}
}
