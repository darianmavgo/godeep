package spihid

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"godeep/kernel/input"
)

type contact struct {
	x, y  int16
	major uint16
}

// tpReport builds a multitouch report: mouse header, touch header, contacts.
func tpReport(buttons byte, cs ...contact) []byte {
	b := make([]byte, tpContactsAt+tpContact*len(cs))
	b[0] = tpReportID
	b[tpCountAt] = byte(len(cs))
	b[tpButtonsAt] = buttons
	le := binary.LittleEndian
	for i, c := range cs {
		f := b[tpContactsAt+i*tpContact:]
		le.PutUint16(f[4:], uint16(c.x))
		le.PutUint16(f[6:], uint16(c.y))
		le.PutUint16(f[18:], c.major)
	}
	return b
}

func collect(ev <-chan input.Event) []string {
	var out []string
	for {
		select {
		case e := <-ev:
			out = append(out, e.String())
		case <-time.After(50 * time.Millisecond):
			return out
		}
	}
}

func TestTrackpadReports(t *testing.T) {
	b := &input.Broadcaster{Kind: "trackpad"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ev := b.Subscribe(ctx)
	tp := Trackpad{Events: b}
	now := time.Unix(0, 0)
	feed := func(p []byte) {
		t.Helper()
		if ok, _ := tp.Report(p, now); !ok {
			t.Fatalf("report rejected: % x", p[:8])
		}
	}

	// mouse mode: relative motion and a click in the 6-byte header
	feed([]byte{tpReportID, 1, 5, 0xfe, 0, 0})
	feed([]byte{tpReportID, 0, 0, 0, 0, 0})
	got := strings.Join(collect(ev), "; ")
	if got != "button 0 down; motion dx=5 dy=-2; button 0 up" {
		t.Fatalf("mouse mode: %s", got)
	}

	// one finger: down, move right/up by (80, 40) units → (10, -5)
	feed(tpReport(0, contact{100, 200, 300}))
	feed(tpReport(0, contact{180, 240, 300}))
	feed(tpReport(0)) // lifted
	got = strings.Join(collect(ev), "; ")
	if !strings.Contains(got, "motion dx=10 dy=-5") || !strings.Contains(got, "touch 0 up x=180 y=240") {
		t.Fatalf("one finger: %s", got)
	}
	if !tp.touchMode {
		t.Fatal("multitouch mode not noticed")
	}

	// two fingers moving away from the user by 2.5 wheel steps → 2 clicks
	feed(tpReport(0, contact{0, 0, 300}, contact{500, 0, 300}))
	feed(tpReport(0, contact{0, 1000, 300}, contact{500, 1000, 300}))
	got = strings.Join(collect(ev), "; ")
	if strings.Count(got, "wheel dx=0 dy=1") != 2 || strings.Contains(got, "motion") {
		t.Fatalf("two-finger scroll: %s", got)
	}

	// click in the touch header; a contact with touch major 0 isn't touching
	feed(tpReport(1, contact{5, 5, 0}))
	got = strings.Join(collect(ev), "; ")
	if !strings.Contains(got, "button 0 down") || strings.Contains(got, "touch 0 down") {
		t.Fatalf("click: %s", got)
	}

	if ok, reset := tp.Report([]byte{tpResetID, 0}, now); !ok || !reset {
		t.Fatal("reset report not recognized")
	}
	if ok, _ := tp.Report([]byte{0x99, 0, 0, 0, 0, 0}, now); ok {
		t.Fatal("unknown report accepted")
	}
}

func TestWritePacket(t *testing.T) {
	b := WritePacket(DevTP, MsgSetReport, 0x02, 0, 7, 2, mtEnable)
	p, ok := ParsePacket(b)
	if !ok || !p.CRCOK || p.Flags != FlagWrite || p.Device != DevTP || p.Length != 12 {
		t.Fatalf("packet %+v", p)
	}
	m, ok := ParseMessage(p.Flags, p.Device, p.Data)
	if !ok || m.Type != MsgSetReport || m.Sub != 0x02 || m.ID != 7 || string(m.Payload) != "\x02\x01" {
		t.Fatalf("message %+v", m)
	}
	if binary.LittleEndian.Uint16(p.Data[4:]) != 2 {
		t.Fatal("rsplen")
	}
	if crc := binary.LittleEndian.Uint16(p.Data[10:]); crc != CRC16(p.Data[:10]) {
		t.Fatal("message CRC")
	}
}

// tpPackets splits a trackpad input message over as many packets as needed.
func tpPackets(report []byte) [][]byte {
	le := binary.LittleEndian
	msg := make([]byte, dataOffset+len(report)+2)
	msg[0], msg[1] = MsgReport, DevTP
	le.PutUint16(msg[6:], uint16(len(report)))
	copy(msg[dataOffset:], report)
	le.PutUint16(msg[dataOffset+len(report):], CRC16(msg[:dataOffset+len(report)]))
	var out [][]byte
	for off := 0; off < len(msg); off += 246 {
		n := min(246, len(msg)-off)
		b := make([]byte, PacketSize)
		b[0], b[1] = FlagRead, DevTP
		le.PutUint16(b[2:], uint16(off))
		le.PutUint16(b[4:], uint16(len(msg)-off-n))
		le.PutUint16(b[6:], uint16(n))
		copy(b[dataOffset:], msg[off:off+n])
		le.PutUint16(b[PacketSize-2:], CRC16(b[:PacketSize-2]))
		out = append(out, b)
	}
	return out
}

func TestTrackpadDriver(t *testing.T) {
	m := newMachine(true)
	cfg, err := ConfigFromFDT(t6000Tree(t).Find("/soc/spi@39b10c000/hid-transport@0"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Keys = make(chan byte, 64)
	cfg.Trackpad = true
	mgr, cancel := startKeyboard(t, m, cfg)
	defer cancel()
	svc, err := mgr.Get(context.Background(), "hid-transport@0")
	if err != nil {
		t.Fatal(err)
	}
	srcs := input.All(svc)
	if len(srcs) != 2 || srcs[1].InputKind() != "trackpad" {
		t.Fatalf("sources %v", srcs)
	}
	ctx, done := context.WithCancel(context.Background())
	defer done()
	ev := srcs[1].Subscribe(ctx)

	booted := make([]byte, PacketSize)
	copy(booted, []byte{FlagRead, DevMngt, 0, 0, 0, 0, 4, 0, 0xa0, 0x80, 0, 0})
	m.send(booted)
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		n := len(m.writes)
		m.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("multitouch set-report never sent")
		}
		time.Sleep(time.Millisecond)
	}
	m.mu.Lock()
	w := m.writes[0]
	m.mu.Unlock()
	if p, _ := ParsePacket(w); !p.CRCOK || p.Device != DevTP || p.Data[0] != MsgSetReport || string(p.Data[8:10]) != "\x02\x01" {
		t.Fatalf("set-report packet % x", w[:24])
	}

	// eight fingers: the message needs two packets
	var cs []contact
	for i := 0; i < 8; i++ {
		cs = append(cs, contact{int16(100 * i), 0, 300})
	}
	m.send(tpPackets(tpReport(0, cs...))...)
	m.send(tpPackets(tpReport(0))...)
	var got []string
	for len(got) < 16 {
		select {
		case e := <-ev:
			got = append(got, e.String())
		case <-time.After(5 * time.Second):
			t.Fatalf("events so far: %v", got)
		}
	}
	all := strings.Join(got, "; ")
	for i := 0; i < 8; i++ {
		if !strings.Contains(all, fmt.Sprintf("touch %d down x=%d", i, 100*i)) || !strings.Contains(all, fmt.Sprintf("touch %d up", i)) {
			t.Fatalf("contact %d missing: %s", i, all)
		}
	}
	info := strings.Join(svc.(interface{ Devices() []string }).Devices(), "; ")
	for _, want := range []string{"trackpad: 2 reports (multitouch)", "multitouch on: 1 sent, status ok", "booted packets 1"} {
		if !strings.Contains(info, want) {
			t.Fatalf("lsdev info %q lacks %q", info, want)
		}
	}
}
