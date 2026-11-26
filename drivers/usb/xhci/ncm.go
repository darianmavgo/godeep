package xhci

import (
	"context"
	"encoding/binary"
	"fmt"

	"godeep/drivers/usb"
)

// CDC NCM (USB Network Control Model 1.0): frames travel in NTBs (NCM
// Transfer Blocks). An NTB16 is a header (NTH16) pointing at a datagram
// pointer table (NDP16) that lists (offset, length) pairs, ended by a
// zero pair; tables can chain. godeep sends one frame per NTB and accepts
// any number per NTB in.

// NCM class requests.
const (
	reqGetNTBParameters = 0x80
	reqSetNTBFormat     = 0x84
	reqSetNTBInputSize  = 0x86

	nth16Sig     = 0x484d434e // "NCMH"
	ndp16Sig     = 0x304d434e // "NCM0" (no CRC)
	ndp16SigCRC  = 0x314d434e // "NCM1"
	nth16Len     = 12
	ndp16MinLen  = 16
	ncmRxDefault = 16384 // NTB input size we ask for
	ncmRxMax     = 65536
)

// ncmFraming holds the device's NTB parameters.
type ncmFraming struct {
	seq      uint16
	inSize   int // NTBs in are at most this long
	outMax   int // dwNtbOutMaxSize
	div, rem int // a datagram out starts at offset ≡ rem (mod div)
	align    int // the NDP out starts at a multiple of align
}

func (*ncmFraming) name() string  { return "CDC NCM" }
func (f *ncmFraming) rxSize() int { return f.inSize }
func (f *ncmFraming) txSize() int { return 4096 }

// parseNTBParameters decodes GET_NTB_PARAMETERS' 28 bytes.
func parseNTBParameters(b []byte) (*ncmFraming, error) {
	if len(b) < 28 {
		return nil, fmt.Errorf("NTB parameters: %d bytes", len(b))
	}
	le16 := func(o int) int { return int(binary.LittleEndian.Uint16(b[o:])) }
	le32 := func(o int) int { return int(binary.LittleEndian.Uint32(b[o:])) }
	f := &ncmFraming{inSize: le32(4), outMax: le32(16), div: le16(20), rem: le16(22), align: le16(24)}
	if f.div == 0 || f.div&(f.div-1) != 0 {
		f.div = 4
	}
	f.rem %= f.div
	if f.align < 4 || f.align&(f.align-1) != 0 {
		f.align = 4
	}
	if f.outMax == 0 || f.outMax > 4096 {
		f.outMax = 4096
	}
	return f, nil
}

func alignTo(x, a int) int { return (x + a - 1) / a * a }

// wrap builds a one-frame NTB16: NTH16, NDP16, then the datagram.
func (f *ncmFraming) wrap(dst, frame []byte) int {
	ndp := alignTo(nth16Len, f.align)
	data := ndp + ndp16MinLen
	data = alignTo(data-f.rem, f.div) + f.rem // offset ≡ rem (mod div)
	if data < ndp+ndp16MinLen {
		data += f.div
	}
	total := data + len(frame)
	clear(dst[:data])
	le16 := func(o, v int) { binary.LittleEndian.PutUint16(dst[o:], uint16(v)) }
	binary.LittleEndian.PutUint32(dst[0:], nth16Sig)
	le16(4, nth16Len)
	le16(6, int(f.seq))
	le16(8, total)
	le16(10, ndp)
	f.seq++
	binary.LittleEndian.PutUint32(dst[ndp:], ndp16Sig)
	le16(ndp+4, ndp16MinLen)
	le16(ndp+6, 0) // no next NDP
	le16(ndp+8, data)
	le16(ndp+10, len(frame))
	// ndp+12: the zero entry that ends the table (cleared above)
	copy(dst[data:], frame)
	return total
}

// unwrap hands every datagram of an NTB16 to emit. Malformed blocks are
// dropped whole or from the bad entry on; it never reads outside b.
func (f *ncmFraming) unwrap(b []byte, emit func([]byte)) {
	if len(b) < nth16Len || binary.LittleEndian.Uint32(b) != nth16Sig {
		return
	}
	le16 := func(o int) int { return int(binary.LittleEndian.Uint16(b[o:])) }
	ndp := le16(10)
	if bl := le16(8); bl >= nth16Len && bl < len(b) {
		b = b[:bl]
	}
	for hops := 0; ndp != 0 && hops < 16; hops++ {
		if ndp%4 != 0 || ndp+ndp16MinLen > len(b) {
			return
		}
		sig := binary.LittleEndian.Uint32(b[ndp:])
		if sig != ndp16Sig && sig != ndp16SigCRC {
			return
		}
		l := le16(ndp + 4)
		if l < ndp16MinLen || ndp+l > len(b) {
			return
		}
		for e := ndp + 8; e+4 <= ndp+l; e += 4 {
			off, n := le16(e), le16(e+2)
			if off == 0 || n == 0 {
				break
			}
			if off+n > len(b) {
				return
			}
			if sig == ndp16SigCRC && n >= 4 {
				n -= 4 // CRC-32 after the frame: not checked
			}
			emit(b[off : off+n])
		}
		ndp = le16(ndp + 6)
	}
}

// newNCM sets up an NCM function: NTB parameters, input size, MAC, then the
// data interface.
func (h *hc) newNCM(ctx context.Context, d *device, cfg usb.Config, comm usb.Interface) error {
	buf, err := h.alloc(64, 64)
	if err != nil {
		return err
	}
	ifc := uint16(comm.Number)
	b, err := h.control(ctx, d, usb.Setup{RequestType: 0xa1, Request: reqGetNTBParameters, Index: ifc, Length: 28}, buf)
	if err != nil {
		return fmt.Errorf("NTB parameters: %w", err)
	}
	fr, err := parseNTBParameters(b)
	if err != nil {
		return err
	}
	// NTB16 is the default after reset; say so anyway (some devices insist).
	h.control(ctx, d, usb.Setup{RequestType: 0x21, Request: reqSetNTBFormat, Value: 0, Index: ifc}, nil)
	switch {
	case fr.inSize <= 0:
		fr.inSize = ncmRxDefault
	case fr.inSize > ncmRxDefault:
		// Ask for smaller blocks; if the device refuses, take its size.
		binary.LittleEndian.PutUint32(buf.Bytes, ncmRxDefault)
		if _, err := h.control(ctx, d, usb.Setup{RequestType: 0x21, Request: reqSetNTBInputSize, Index: ifc, Length: 4}, buf); err == nil {
			fr.inSize = ncmRxDefault
		} else if fr.inSize > ncmRxMax {
			return fmt.Errorf("NTB input size %d", fr.inSize)
		}
	}
	n := newNIC(h, d, fr)
	if n.mac, err = h.readMAC(ctx, d, comm); err != nil {
		return err
	}
	data, err := cdcData(cfg, comm)
	if err != nil {
		return err
	}
	if err := h.startNIC(ctx, n, *data); err != nil {
		return err
	}
	h.control(ctx, d, usb.Setup{RequestType: 0x21, Request: reqSetFilter, Value: 0x0e, Index: ifc}, nil)
	h.addNIC(n)
	return nil
}
