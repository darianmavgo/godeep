package xhci

import (
	"bytes"
	"encoding/binary"
	"testing"

	"godeep/drivers/usb"
	"godeep/kernel/dev"
)

func TestNCMRoundTrip(t *testing.T) {
	params := make([]byte, 28)
	binary.LittleEndian.PutUint32(params[4:], 16384) // in max
	binary.LittleEndian.PutUint32(params[16:], 2048) // out max
	binary.LittleEndian.PutUint16(params[20:], 4)    // divisor
	binary.LittleEndian.PutUint16(params[22:], 2)    // remainder
	binary.LittleEndian.PutUint16(params[24:], 4)    // NDP alignment
	f, err := parseNTBParameters(params)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{14, 60, 1514} {
		frame := bytes.Repeat([]byte{byte(n)}, n)
		buf := make([]byte, f.txSize())
		l := f.wrap(buf, frame)
		data := int(binary.LittleEndian.Uint16(buf[12+8:]))
		if data%4 != 2 {
			t.Fatalf("datagram at %d, want ≡ 2 mod 4", data)
		}
		var got [][]byte
		f.unwrap(buf[:l], func(b []byte) { got = append(got, append([]byte(nil), b...)) })
		if len(got) != 1 || !bytes.Equal(got[0], frame) {
			t.Fatalf("%d-byte frame came back as %d frames", n, len(got))
		}
	}
}

// Two NDPs chained, two datagrams in the first.
func TestNCMUnwrapChained(t *testing.T) {
	b := make([]byte, 512)
	le16 := func(o, v int) { binary.LittleEndian.PutUint16(b[o:], uint16(v)) }
	binary.LittleEndian.PutUint32(b, nth16Sig)
	le16(4, 12)
	le16(8, 512)
	le16(10, 16)
	binary.LittleEndian.PutUint32(b[16:], ndp16Sig)
	le16(20, 20)  // three entries + terminator... two + zero
	le16(22, 64)  // next NDP
	le16(24, 100) // datagram 1
	le16(26, 20)
	le16(28, 200)
	le16(30, 30)
	binary.LittleEndian.PutUint32(b[64:], ndp16Sig)
	le16(68, 16)
	le16(72, 300)
	le16(74, 40)
	var lens []int
	(&ncmFraming{}).unwrap(b, func(f []byte) { lens = append(lens, len(f)) })
	if len(lens) != 3 || lens[0] != 20 || lens[1] != 30 || lens[2] != 40 {
		t.Fatalf("got %v", lens)
	}
}

func FuzzNCMUnwrap(f *testing.F) {
	f.Add([]byte("NCMH\x0c\x00\x00\x00\x20\x00\x0c\x00NCM0\x10\x00\x00\x00\x1c\x00\x04\x00\x00\x00\x00\x00abcd"))
	f.Fuzz(func(t *testing.T, b []byte) {
		(&ncmFraming{}).unwrap(b, func([]byte) {})
	})
}

func TestChildTT(t *testing.T) {
	hs := &device{slot: 3, speed: usb.High}
	if got := hs.childTT(2, usb.Low); got != (tt{3, 2}) {
		t.Fatalf("LS behind HS hub: %+v", got)
	}
	if got := hs.childTT(2, usb.High); got != (tt{}) {
		t.Fatalf("HS behind HS hub: %+v", got)
	}
	fs := &device{slot: 5, speed: usb.Full, tt: tt{3, 4}}
	if got := fs.childTT(1, usb.Full); got != (tt{3, 4}) {
		t.Fatalf("FS behind FS hub behind HS hub: %+v", got)
	}
	if got := (tt{3, 4}).slotWord(); got != 0x0403 {
		t.Fatalf("slot word %#x", got)
	}
}

// Names: the lowest free name, kept while in use, reused once released;
// a name another controller holds is skipped.
func TestNames(t *testing.T) {
	taken := map[string]bool{"usbnet1": true} // another controller's
	add := func(n dev.Node) bool {
		if taken[n.Name] {
			return false
		}
		taken[n.Name] = true
		return true
	}
	var n Names
	a := n.publish(add, "usbnet", dev.Node{})
	b := n.publish(add, "usbnet", dev.Node{})
	if a != "usbnet0" || b != "usbnet2" {
		t.Fatalf("got %s %s", a, b)
	}
	n.release(a)
	if c := n.publish(add, "usbnet", dev.Node{}); c != "usbnet0" {
		t.Fatalf("after release: %s", c)
	}
	n.release(b)
	n.release("usbnet0")
	if c := n.publish(add, "usbnet", dev.Node{}); c != "usbnet0" {
		t.Fatalf("a new run: %s", c)
	}
}
