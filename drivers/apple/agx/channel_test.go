package agx

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"godeep/drivers/apple/agx/fw"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

func newKernelAlloc(t *testing.T) (*KernelAlloc, *PageTable) {
	t.Helper()
	a := dma.New(dma.Region{Base: 0x108_0000_0000, Mem: make([]byte, 16<<20)})
	pt, err := BorrowPageTable(mmio.NewFake(), &DMAPages{A: a}, 0x100_0000_0000, T6000.OAS, KernBase, KernTop)
	if err != nil {
		t.Fatal(err)
	}
	return NewKernelAlloc(pt, a), pt
}

func TestKernelAlloc(t *testing.T) {
	k, pt := newKernelAlloc(t)
	o, err := k.Alloc(Shared, 100)
	if err != nil {
		t.Fatal(err)
	}
	if o.VA != 0xffffffa800000000 || len(o.Bytes()) != PageSize {
		t.Fatalf("object at %#x, %d bytes", o.VA, len(o.Bytes()))
	}
	pa, pte, ok := pt.Lookup(o.VA)
	if !ok || pa != o.Buf.Phys {
		t.Fatalf("not mapped: %#x %v", pa, ok)
	}
	if pte>>attrShift&7 != attrUncached || pte>>apShift&3 != apFW {
		t.Fatalf("shared object pte %#x", pte)
	}
	// Firmware-private memory is uncached on godeep too (see classes).
	p, _ := k.Alloc(Private, 1)
	if _, pte, _ := pt.Lookup(p.VA); pte>>attrShift&7 != attrUncached {
		t.Fatalf("private pte %#x is cached", pte)
	}
	// Objects never share a page, and a guard page separates them.
	o2, _ := k.Alloc(Shared, 1)
	if o2.VA != o.VA+2*PageSize {
		t.Fatalf("second object at %#x", o2.VA)
	}
	if _, _, ok := pt.Lookup(o.VA + PageSize); ok {
		t.Fatal("guard page mapped")
	}
	// RTKit buffers land in their own range.
	a := dma.New(dma.Region{Base: 0x109_0000_0000, Mem: make([]byte, 1<<20)})
	b, _ := a.Alloc(PageSize, PageSize)
	va, err := (RTKitIOMMU{k}).Map(b, dma.Bidirectional)
	if err != nil || va != 0xffffffae00000000 {
		t.Fatalf("rtkit buffer at %#x: %v", va, err)
	}
	if err := (RTKitIOMMU{k}).Unmap(va, PageSize); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := pt.Lookup(va); ok {
		t.Fatal("rtkit buffer still mapped")
	}
	if k.Used() == "none" {
		t.Fatal("usage not counted")
	}
}

func TestChannels(t *testing.T) {
	k, _ := newKernelAlloc(t)
	ctx := context.Background()

	// Transmit: messages land in order; the write pointer follows.
	tx, err := NewTx(k, ChannelState, 4, DeviceControlMsgSize)
	if err != nil {
		t.Fatal(err)
	}
	w, err := tx.Put(ctx, []byte{1, 2, 3})
	if err != nil || w != 1 || tx.State.Buf.Load32(0x20) != 1 {
		t.Fatalf("put: %d %v, write_ptr %d", w, err, tx.State.Buf.Load32(0x20))
	}
	if m := tx.Msgs.Bytes(); m[0] != 1 || m[2] != 3 || m[3] != 0 {
		t.Fatalf("slot 0: % x", m[:4])
	}
	// The firmware consumes it.
	go func() { time.Sleep(5 * time.Millisecond); tx.State.Buf.Store32(0, 1) }()
	if err := tx.WaitFor(ctx, w, time.Second); err != nil {
		t.Fatal(err)
	}
	// Full: 3 of 4 slots usable; the 4th Put waits for the firmware.
	tx.Put(ctx, []byte{2})
	tx.Put(ctx, []byte{3})
	tx.Put(ctx, []byte{4})
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := tx.Put(short, []byte{5}); err == nil {
		t.Fatal("put into a full ring")
	}
	if _, err := tx.Put(ctx, make([]byte, DeviceControlMsgSize+1)); err == nil {
		t.Fatal("oversized message accepted")
	}

	// Receive on sub-channel 2 of 6 (the firmware log).
	rx, err := NewRx(k, ChannelState, 6, 4, FwLogMsgSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rx.Get(2); ok {
		t.Fatal("message from an empty channel")
	}
	off := (2*4 + 0) * FwLogMsgSize
	rx.Msgs.Bytes()[off] = 0x42
	rx.State.Buf.Store32(2*0x30+0x20, 1) // firmware: write_ptr = 1
	m, ok := rx.Get(2)
	if !ok || m[0] != 0x42 || rx.State.Buf.Load32(2*0x30) != 1 {
		t.Fatalf("get: %v % x, read_ptr %d", ok, m[:1], rx.State.Buf.Load32(2*0x30))
	}
	if _, ok := rx.Get(2); ok {
		t.Fatal("message read twice")
	}
	rx.State.Buf.Store32(2*0x30+0x20, 99) // out of range: ignored, not indexed
	if _, ok := rx.Get(2); ok {
		t.Fatal("garbage write pointer used")
	}

	chs, err := NewChannels(k, [3]uint32{13, 5, 0})
	if err != nil {
		t.Fatal(err)
	}
	if chs.Stats.size != 0x40 || chs.Pipes[3][2] == nil || chs.FwLog.subs != 6 {
		t.Fatal("channel set incomplete")
	}
}

func TestLayouts(t *testing.T) {
	for _, compat := range [][3]uint32{{12, 3, 0}, {13, 5, 0}} {
		l, err := fw.For(13, compat)
		if err != nil {
			t.Fatal(err)
		}
		// The ring states and messages agree with the hand-written sizes.
		if s := l.Get("channels.ChannelState"); s.Size != ChannelState.Size || s.Off("read_ptr") != 0 || s.Off("write_ptr") != 0x20 {
			t.Fatalf("%v ChannelState %+v", compat, s)
		}
		if s := l.Get("channels.FwCtlChannelState"); s.Size != FwCtlChannelState.Size || s.Off("write_ptr") != 0x10 {
			t.Fatalf("%v FwCtlChannelState", compat)
		}
		for name, size := range map[string]int{"channels.RawFwLogMsg": FwLogMsgSize, "channels.RawKTraceMsg": KTraceMsgSize,
			"channels.FwCtlMsg": FwCtlMsgSize, "channels.RunWorkQueueMsg": PipeMsgSize} {
			if l.Get(name).Size != size {
				t.Fatalf("%v %s is %#x, want %#x", compat, name, l.Get(name).Size, size)
			}
		}
		if l.Get("initdata.ChannelRing").Size != 16 {
			t.Fatal("ChannelRing is two pointers")
		}
	}
	// Spot checks against offsets named in the source and m1n1's notes.
	l, _ := fw.For(13, [3]uint32{12, 3, 0})
	if l.Get("initdata.RuntimePointers").Size != 0x6bc0 { // m1n1: regionB, 0x6bc0
		t.Fatal("RuntimePointers size")
	}
	if a := l.Get("initdata.HwDataA"); a.Off("unk_1cc") != 0x1cc || a.Off("pad_644") != 0x644 {
		t.Fatal("HwDataA offsets")
	}
	// 13.5 inserts fields: the same field moves.
	l13, _ := fw.For(13, [3]uint32{13, 5, 0})
	if l13.Get("initdata.HwDataA").Off("unk_1cc") <= 0x1cc || !l13.Get("initdata.HwDataA").Has("clocks_per_period_2") ||
		l.Get("initdata.HwDataA").Has("clocks_per_period_2") {
		t.Fatal("13.5 fields")
	}
	if _, err := fw.For(13, [3]uint32{14, 0, 0}); err == nil {
		t.Fatal("unknown firmware has layouts")
	}
	// Writes by name; writes to a field this version lacks go nowhere; a
	// field no version has (a typo) panics, as does a float write into an
	// integer field or an index past the array.
	buf := make([]byte, l.Get("initdata.HwDataA").Size)
	w := l.Writer("initdata.HwDataA", buf)
	w.Set("unk_0", 7)
	w.Set("clocks_per_period_2", 9) // 13.x only: dropped for 12.3
	w.F32At("sram_k", 1, 1.5)
	w.Set("unk_10", 2) // an F32 field: written as 2.0
	if buf[0] != 7 || binary.LittleEndian.Uint32(buf[l.Get("initdata.HwDataA").Off("unk_10"):]) != math.Float32bits(2) {
		t.Fatal("writer")
	}
	for _, bad := range []func(){
		func() { w.Set("clocks_per_period_3", 1) },
		func() { w.F32("unk_0", 1) },
		func() { w.SetAt("sram_k", 16, 1) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("a bad write was accepted")
				}
			}()
			bad()
		}()
	}
}
