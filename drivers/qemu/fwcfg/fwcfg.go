// Package fwcfg talks to QEMU's firmware configuration device
// ("qemu,fw-cfg-mmio" on the virt machine): a directory of named items the
// guest can read and, for a few, write. godeep uses it for one thing so
// far: ramfb, a framebuffer in guest RAM that QEMU displays (and that
// screendump captures), configured by writing the item "etc/ramfb".
//
// Everything goes through the DMA interface. The selector register only
// accepts 16-bit accesses, and godeep's registers are 32-bit only
// (mmio.Regs); a DMA request can select an item itself. The request is a
// big-endian structure in DMA memory; writing its address to the DMA
// register (high word, then low word, which starts it) runs it, and the
// device clears the control word when done.
package fwcfg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"strings"
	"time"

	"godeep/kernel/dma"
	"godeep/kernel/mmio"
)

// Compatible is the device-tree compatible of the MMIO interface.
const Compatible = "qemu,fw-cfg-mmio"

// Registers (virt machine).
const (
	RegDMAHigh = 0x10 // big-endian 64-bit DMA address: high word
	RegDMALow  = 0x14 // low word: writing it starts the request
)

// DMA control bits.
const (
	CtlError  = 0x01
	CtlRead   = 0x02
	CtlSkip   = 0x04
	CtlSelect = 0x08
	CtlWrite  = 0x10
)

// KeyFileDir is the item listing every named file.
const KeyFileDir = 0x19

const (
	reqSize  = 16
	bufSize  = 4096
	dirEntry = 64
)

// FwCfg is a fw_cfg device. Not safe for concurrent use.
type FwCfg struct {
	r   mmio.Regs
	buf *dma.Buffer // request at 0, data after it
}

// File is a named item.
type File struct {
	Name string
	Key  uint16
	Size int
}

// New prepares the interface; Close frees its buffer.
func New(r mmio.Regs, a *dma.Allocator) (*FwCfg, error) {
	if a == nil {
		return nil, errors.New("fwcfg: no DMA region")
	}
	b, err := a.Alloc(reqSize+bufSize, dma.DefaultAlign)
	if err != nil {
		return nil, err
	}
	return &FwCfg{r: r, buf: b}, nil
}

func (f *FwCfg) Close() { f.buf.Free() }

// xfer runs one DMA request on item key over n bytes at the data area.
func (f *FwCfg) xfer(key uint16, ctl uint32, n int) error {
	if n > bufSize {
		return fmt.Errorf("fwcfg: %d bytes is more than one transfer", n)
	}
	req := f.buf.Bytes[:reqSize]
	be := binary.BigEndian
	be.PutUint32(req[4:], uint32(n))
	be.PutUint64(req[8:], f.buf.Phys+reqSize)
	// The control word goes last, with release ordering.
	f.buf.Store32(0, bits.ReverseBytes32(uint32(key)<<16|CtlSelect|ctl))
	addr := f.buf.Phys
	f.r.Write32(RegDMAHigh, bits.ReverseBytes32(uint32(addr>>32)))
	f.r.Write32(RegDMALow, bits.ReverseBytes32(uint32(addr)))
	deadline := time.Now().Add(time.Second)
	for {
		c := bits.ReverseBytes32(f.buf.Load32(0))
		if c&CtlError != 0 {
			return fmt.Errorf("fwcfg: item %#x: device reported an error", key)
		}
		if c == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("fwcfg: item %#x: request timed out", key)
		}
	}
}

// Read reads the start of item key into p.
func (f *FwCfg) Read(key uint16, p []byte) error {
	if err := f.xfer(key, CtlRead, len(p)); err != nil {
		return err
	}
	copy(p, f.buf.Bytes[reqSize:reqSize+len(p)])
	return nil
}

// Write writes p to item key (only a few items, like etc/ramfb, accept it).
func (f *FwCfg) Write(key uint16, p []byte) error {
	copy(f.buf.Bytes[reqSize:], p)
	return f.xfer(key, CtlWrite, len(p))
}

// Files lists the named items.
func (f *FwCfg) Files() ([]File, error) {
	var n [4]byte
	if err := f.Read(KeyFileDir, n[:]); err != nil {
		return nil, err
	}
	count := int(binary.BigEndian.Uint32(n[:]))
	if count > (bufSize-4)/dirEntry {
		count = (bufSize - 4) / dirEntry // ramfb is early in the list; enough
	}
	dir := make([]byte, 4+count*dirEntry)
	if err := f.Read(KeyFileDir, dir); err != nil {
		return nil, err
	}
	var out []File
	for i := 0; i < count; i++ {
		e := dir[4+i*dirEntry:][:dirEntry]
		out = append(out, File{
			Size: int(binary.BigEndian.Uint32(e[0:])),
			Key:  binary.BigEndian.Uint16(e[4:]),
			Name: strings.TrimRight(string(e[8:]), "\x00"),
		})
	}
	return out, nil
}

// Find returns the named item.
func (f *FwCfg) Find(name string) (File, error) {
	files, err := f.Files()
	if err != nil {
		return File{}, err
	}
	for _, fl := range files {
		if fl.Name == name {
			return fl, nil
		}
	}
	return File{}, fmt.Errorf("fwcfg: no %q (QEMU needs -device %s)", name, strings.TrimPrefix(name, "etc/"))
}

// FourCCXRGB8888 is DRM's "XR24": 32 bits per pixel, x:r:g:b, little-endian.
const FourCCXRGB8888 = 'X' | 'R'<<8 | '2'<<16 | '4'<<24

// RAMFB is a framebuffer QEMU shows (-device ramfb).
type RAMFB struct {
	Pix           []byte
	Width, Height int
	Stride        int
	mem           *dma.Buffer
}

// SetupRAMFB allocates width x height XRGB8888 pixels in DMA memory and
// hands them to QEMU's ramfb. It fails if QEMU has no ramfb device.
func (f *FwCfg) SetupRAMFB(a *dma.Allocator, width, height int) (*RAMFB, error) {
	file, err := f.Find("etc/ramfb")
	if err != nil {
		return nil, err
	}
	stride := width * 4
	mem, err := a.Alloc(stride*height, 4096)
	if err != nil {
		return nil, err
	}
	clear(mem.Bytes)
	cfg := make([]byte, 28)
	be := binary.BigEndian
	be.PutUint64(cfg[0:], mem.Phys)
	be.PutUint32(cfg[8:], FourCCXRGB8888)
	be.PutUint32(cfg[12:], 0) // flags
	be.PutUint32(cfg[16:], uint32(width))
	be.PutUint32(cfg[20:], uint32(height))
	be.PutUint32(cfg[24:], uint32(stride))
	if err := f.Write(file.Key, cfg); err != nil {
		mem.Free()
		return nil, err
	}
	return &RAMFB{Pix: mem.Bytes[:stride*height], Width: width, Height: height, Stride: stride, mem: mem}, nil
}
