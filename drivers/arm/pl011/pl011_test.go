package pl011

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/mmio"
	"godeep/kernel/proc"
)

// uart models the receive side: queued bytes come out of DR one at a time.
type uart struct {
	mu sync.Mutex
	f  *mmio.Fake
	rx []byte
}

func newUART() *uart {
	u := &uart{f: mmio.NewFake()}
	u.f.OnRead = func(f *mmio.Fake, off uint64) (uint32, bool) {
		u.mu.Lock()
		defer u.mu.Unlock()
		switch off {
		case FR:
			if len(u.rx) == 0 {
				return FR_RXFE, true
			}
			return 0, true
		case DR:
			b := u.rx[0]
			u.rx = u.rx[1:]
			return uint32(b), true
		}
		return 0, false
	}
	return u
}

func (u *uart) send(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rx = append(u.rx, s...)
}

func TestInput(t *testing.T) {
	u := newUART()
	dev.Register(Driver(func(uint64) mmio.Regs { return u.f }))
	in := make(chan byte, 4)
	m := dev.NewManager(proc.NewTable(), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx, []dev.Node{{Name: "serial", Compatible: Compatible, Reg: []dev.Range{{Base: 0x60000000, Size: 0x1000}},
		Config: (chan<- byte)(in)}})
	svc, err := m.Get(ctx, "serial")
	if err != nil {
		t.Fatal(err)
	}
	if cr := u.f.Get(CR); cr != CR_UARTEN|CR_TXE|CR_RXE {
		t.Fatalf("CR %#x: receiver not enabled", cr)
	}
	u.send("help\r")
	// Nobody reads yet: the channel holds 4 bytes, the 5th is dropped.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(svc.(dev.Lister).Devices()[0], "4 bytes, 1 dropped") {
		if time.Now().After(deadline) {
			t.Fatalf("stats: %v", svc.(dev.Lister).Devices())
		}
		time.Sleep(time.Millisecond)
	}
	if got := string([]byte{<-in, <-in, <-in, <-in}); got != "help" {
		t.Fatalf("got %q", got)
	}
}

func TestNoChannel(t *testing.T) {
	dev.Register(Driver(func(uint64) mmio.Regs { return mmio.NewFake() }))
	m := dev.NewManager(proc.NewTable(), nil, nil)
	m.Backoff = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx, []dev.Node{{Name: "serial", Compatible: Compatible, Reg: []dev.Range{{Base: 1}}}})
	if _, err := m.Get(ctx, "serial"); err == nil || !strings.Contains(err.Error(), "no input channel") {
		t.Fatalf("err = %v", err)
	}
}
