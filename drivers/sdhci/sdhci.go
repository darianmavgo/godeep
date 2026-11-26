// Package sdhci drives an SD Host Controller (the standard register set:
// QEMU's sdhci-pci; the Mac's Genesys GL9755 card reader behind PCIe) with
// an SD card in it, polled and by PIO: card identification, 4-bit bus at
// 25 MHz, single and multi-block reads and writes. The card is published
// as a block.Device (block.Writer unless it's write-protected), so lsblk,
// partitions and FAT/ext4 work as for any disk.
//
// One goroutine owns the controller; requests come over a channel. Every
// register access is a single aligned 32-bit access: the 8- and 16-bit
// registers are reached through the 32-bit word that holds them, and the
// command is written together with the transfer mode (writing the command
// half issues it).
package sdhci

import (
	"context"
	"errors"
	"fmt"
	"time"

	"godeep/drivers/pci"
	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/mmio"
)

// Compatibles: SD host controllers by PCI class (no DMA / SDMA).
const (
	Compatible    = "pciclass,080500"
	CompatibleDMA = "pciclass,080501"
)

// Registers (32-bit words).
const (
	BLOCK          = 0x04 // block size (low 16) | block count (high 16)
	ARGUMENT       = 0x08
	XFER_CMD       = 0x0c // transfer mode (low 16) | command (high 16)
	RESPONSE       = 0x10 // 0x10..0x1c
	BUFFER         = 0x20
	PRESENT        = 0x24
	HOST_CTRL      = 0x28 // host control (byte 0), power (byte 1)
	CLOCK          = 0x2c // clock control (low 16), timeout (byte 2), reset (byte 3)
	INT_STATUS     = 0x30 // normal (low 16) | error (high 16), write 1 to clear
	INT_ENABLE     = 0x34
	SIGNAL_ENABLE  = 0x38
	CAPABILITIES   = 0x40
	HOST_VERSION   = 0xfc // high 16
	presentInhibit = 1<<0 | 1<<1
	presentCard    = 1 << 16
	presentWP      = 1 << 19 // write enabled when set

	intCmdDone   = 1 << 0
	intXferDone  = 1 << 1
	intWriteRdy  = 1 << 4
	intReadRdy   = 1 << 5
	intError     = 1 << 15
	errorBits    = 0xffff << 16
	resetAll     = 1 << 24
	resetCmdData = 1<<25 | 1<<26
	clkIntEnable = 1 << 0
	clkIntStable = 1 << 1
	clkCard      = 1 << 2
	power33      = 0x0f << 8 // 3.3 V, on
	hostBus4     = 1 << 1

	// command register (upper half of XFER_CMD)
	rspNone  = 0
	rsp136   = 1
	rsp48    = 2
	rsp48b   = 3
	crcCheck = 1 << 3
	idxCheck = 1 << 4
	dataPres = 1 << 5

	// transfer mode (lower half)
	xferBlockCount = 1 << 1
	xferAutoCMD12  = 1 << 2
	xferRead       = 1 << 4
	xferMulti      = 1 << 5

	BlockSize = 512
	maxBlocks = 128 // per command: 64 KB
	cmdTimout = time.Second
)

// Response kinds of the commands used here.
const (
	r1  = rsp48 | crcCheck | idxCheck
	r1b = rsp48b | crcCheck | idxCheck
	r2  = rsp136 | crcCheck
	r3  = rsp48
	r6  = r1
	r7  = r1
)

// Driver returns the driver for an SD host controller found on PCI.
func Driver(regs func(base uint64) mmio.Regs, compatible string) dev.Driver {
	return dev.Driver{Compatible: compatible, Run: func(ctx context.Context, d *dev.Device) error {
		f, ok := d.Config.(*pci.Function)
		if !ok || f.BARs[0].Size == 0 {
			return errors.New("sdhci: no PCI function with a BAR0")
		}
		return Run(ctx, d, regs(f.BARs[0].CPU))
	}}
}

// Card is the published service.
type Card struct {
	reqs     chan *req
	blocks   uint64
	readOnly bool
	info     string
}

type req struct {
	lba   uint64
	p     []byte
	write bool
	done  chan error
}

func (c *Card) BlockSize() int    { return BlockSize }
func (c *Card) Blocks() uint64    { return c.blocks }
func (c *Card) Devices() []string { return []string{c.info} }
func (c *Card) ReadBlocks(ctx context.Context, lba uint64, p []byte) error {
	return c.do(ctx, lba, p, false)
}

// WriteBlocks implements block.Writer.
func (c *Card) WriteBlocks(ctx context.Context, lba uint64, p []byte) error {
	if c.readOnly {
		return block.ErrReadOnly
	}
	return c.do(ctx, lba, p, true)
}

func (c *Card) do(ctx context.Context, lba uint64, p []byte, write bool) error {
	if err := block.Check(c, lba, p); err != nil {
		return err
	}
	r := &req{lba: lba, p: p, write: write, done: make(chan error, 1)}
	select {
	case c.reqs <- r:
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-r.done
}

type host struct {
	r     mmio.Regs
	base  uint64 // base clock, Hz
	rca   uint32
	sdhc  bool // block addressing
	clock uint32
}

// Run initializes the controller and the card and serves requests until
// ctx is done.
func Run(ctx context.Context, d *dev.Device, r mmio.Regs) error {
	h := &host{r: r}
	c, err := h.init(ctx)
	if err != nil {
		return err
	}
	c.reqs = make(chan *req)
	d.Ready(c)
	for {
		select {
		case <-ctx.Done():
			return nil
		case q := <-c.reqs:
			q.done <- h.transfer(ctx, q.lba, q.p, q.write)
		}
	}
}

func (h *host) wait(ctx context.Context, what string, timeout time.Duration, done func() bool) error {
	deadline := time.Now().Add(timeout)
	for i := 0; !done(); i++ {
		if time.Now().After(deadline) {
			return fmt.Errorf("sdhci: timeout waiting for %s", what)
		}
		if i > 100 {
			t := time.NewTimer(50 * time.Microsecond)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
	}
	return nil
}

func (h *host) reset(ctx context.Context, bits uint32) error {
	h.r.Write32(CLOCK, h.r.Read32(CLOCK)&0x00ffffff|bits)
	return h.wait(ctx, "reset", 100*time.Millisecond, func() bool { return h.r.Read32(CLOCK)&bits == 0 })
}

// setClock runs the card clock at hz or the nearest below (SDHCI 3.0's
// 10-bit divided clock: base / (2*div), div 0 = base).
func (h *host) setClock(ctx context.Context, hz uint64) error {
	div := uint64(0)
	if h.base > hz {
		div = (h.base + 2*hz - 1) / (2 * hz)
	}
	div = min(div, 0x3ff)
	v := h.r.Read32(CLOCK) &^ 0xffff
	h.r.Write32(CLOCK, v) // stop
	ctl := uint32(div&0xff)<<8 | uint32(div>>8&3)<<6 | clkIntEnable
	h.r.Write32(CLOCK, v|ctl)
	if err := h.wait(ctx, "internal clock", 20*time.Millisecond, func() bool { return h.r.Read32(CLOCK)&clkIntStable != 0 }); err != nil {
		return err
	}
	h.r.Write32(CLOCK, v|ctl|clkCard)
	if div == 0 {
		h.clock = uint32(h.base)
	} else {
		h.clock = uint32(h.base / (2 * div))
	}
	return nil
}

// cmd issues a command (no data) and returns its 32-bit response.
func (h *host) cmd(ctx context.Context, idx int, arg uint32, flags uint32) (uint32, error) {
	if err := h.cmdStart(ctx, idx, arg, flags, 0); err != nil {
		return 0, err
	}
	if err := h.waitInt(ctx, intCmdDone, fmt.Sprintf("CMD%d", idx)); err != nil {
		return 0, err
	}
	if flags&3 == rsp48b { // busy: wait for the transfer-complete that ends it
		if err := h.waitInt(ctx, intXferDone, fmt.Sprintf("CMD%d busy", idx)); err != nil {
			return 0, err
		}
	}
	return h.r.Read32(RESPONSE), nil
}

func (h *host) cmdStart(ctx context.Context, idx int, arg, flags, mode uint32) error {
	inhibit := uint32(1 << 0)
	if flags&dataPres != 0 || flags&3 == rsp48b {
		inhibit = presentInhibit
	}
	if err := h.wait(ctx, "command line", cmdTimout, func() bool { return h.r.Read32(PRESENT)&inhibit == 0 }); err != nil {
		return err
	}
	h.r.Write32(INT_STATUS, 0xffffffff)
	h.r.Write32(ARGUMENT, arg)
	h.r.Write32(XFER_CMD, (uint32(idx)<<8|flags)<<16|mode)
	return nil
}

// waitInt waits for an interrupt status bit, failing on any error bit, and
// clears it.
func (h *host) waitInt(ctx context.Context, bit uint32, what string) error {
	var st uint32
	err := h.wait(ctx, what, cmdTimout, func() bool {
		st = h.r.Read32(INT_STATUS)
		return st&(bit|intError) != 0
	})
	if err != nil {
		h.reset(ctx, resetCmdData)
		return err
	}
	if st&intError != 0 {
		h.r.Write32(INT_STATUS, st&errorBits|intError)
		h.reset(ctx, resetCmdData)
		return fmt.Errorf("sdhci: %s: error status %#x", what, st>>16)
	}
	h.r.Write32(INT_STATUS, bit)
	return nil
}

func (h *host) init(ctx context.Context) (*Card, error) {
	r := h.r
	if err := h.reset(ctx, resetAll); err != nil {
		return nil, err
	}
	caps := r.Read32(CAPABILITIES)
	ver := r.Read32(HOST_VERSION) >> 16 & 0xff
	h.base = uint64(caps>>8&0xff) * 1_000_000
	if ver < 2 { // SDHCI 1.0/2.0: 6-bit field
		h.base = uint64(caps>>8&0x3f) * 1_000_000
	}
	if h.base == 0 {
		return nil, errors.New("sdhci: no base clock in the capabilities")
	}
	if r.Read32(PRESENT)&presentCard == 0 {
		return nil, errors.New("sdhci: no card")
	}
	r.Write32(HOST_CTRL, r.Read32(HOST_CTRL)&^0xff00|power33)
	r.Write32(INT_ENABLE, 0xffffffff) // status bits, polled
	r.Write32(SIGNAL_ENABLE, 0)       // no interrupts
	r.Write32(CLOCK, r.Read32(CLOCK)&^0x0f0000|0x0e<<16)
	if err := h.setClock(ctx, 400_000); err != nil {
		return nil, err
	}
	time.Sleep(time.Millisecond) // 74 clocks before the first command

	if _, err := h.cmd(ctx, 0, 0, rspNone); err != nil { // GO_IDLE
		return nil, err
	}
	v2 := true
	if resp, err := h.cmd(ctx, 8, 0x1aa, r7); err != nil || resp&0xfff != 0x1aa { // SEND_IF_COND
		v2 = false
	}
	arg := uint32(0x00ff8000) // 2.7-3.6 V
	if v2 {
		arg |= 1 << 30 // HCS: we take SDHC/SDXC
	}
	var ocr uint32
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := h.cmd(ctx, 55, 0, r1); err != nil {
			return nil, fmt.Errorf("sdhci: not an SD card (CMD55: %w)", err)
		}
		var err error
		if ocr, err = h.cmd(ctx, 41, arg, r3); err != nil {
			return nil, err
		}
		if ocr&(1<<31) != 0 {
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("sdhci: card stays busy (ACMD41)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.sdhc = ocr&(1<<30) != 0
	if _, err := h.cmd(ctx, 2, 0, r2); err != nil { // ALL_SEND_CID
		return nil, err
	}
	cid := h.resp136()
	rca, err := h.cmd(ctx, 3, 0, r6) // SEND_RELATIVE_ADDR
	if err != nil {
		return nil, err
	}
	h.rca = rca &^ 0xffff
	if _, err := h.cmd(ctx, 9, h.rca, r2); err != nil { // SEND_CSD
		return nil, err
	}
	blocks, err := capacity(h.resp136())
	if err != nil {
		return nil, err
	}
	if _, err := h.cmd(ctx, 7, h.rca, r1b); err != nil { // SELECT
		return nil, err
	}
	if _, err := h.cmd(ctx, 55, h.rca, r1); err == nil { // 4-bit bus
		if _, err := h.cmd(ctx, 6, 2, r1); err == nil {
			r.Write32(HOST_CTRL, r.Read32(HOST_CTRL)|hostBus4)
		}
	}
	if !h.sdhc {
		if _, err := h.cmd(ctx, 16, BlockSize, r1); err != nil { // SET_BLOCKLEN
			return nil, err
		}
	}
	if err := h.setClock(ctx, 25_000_000); err != nil {
		return nil, err
	}
	c := &Card{blocks: blocks, readOnly: r.Read32(PRESENT)&presentWP == 0}
	kind := "SDSC"
	if h.sdhc {
		kind = "SDHC/SDXC"
	}
	pnm := bits(cid, 103, 64) // product name: 5 ASCII characters
	name := string([]byte{byte(pnm >> 32), byte(pnm >> 24), byte(pnm >> 16), byte(pnm >> 8), byte(pnm)})
	c.info = fmt.Sprintf("%s card %q, %d MB, %d-bit at %d MHz", kind, name, blocks*BlockSize>>20,
		map[bool]int{true: 4, false: 1}[r.Read32(HOST_CTRL)&hostBus4 != 0], h.clock/1_000_000)
	if c.readOnly {
		c.info += ", write-protected"
	}
	return c, nil
}

// resp136 returns a long response: 120 bits (the CRC is stripped) in four
// words, resp[0] lowest.
func (h *host) resp136() [4]uint32 {
	var v [4]uint32
	for i := range v {
		v[i] = h.r.Read32(RESPONSE + 4*uint64(i))
	}
	return v
}

// bits returns CSD/CID bits [hi:lo] (the register's own numbering) from a
// long response, which holds bits 127:8 shifted down by 8.
func bits(v [4]uint32, hi, lo int) uint64 {
	var x uint64
	for b := hi; b >= lo; b-- {
		i := b - 8
		x = x<<1 | uint64(v[i/32]>>(i%32)&1)
	}
	return x
}

// capacity decodes the card's size in 512-byte blocks from its CSD.
func capacity(csd [4]uint32) (uint64, error) {
	switch bits(csd, 127, 126) {
	case 0: // CSD 1.0 (SDSC)
		cSize, mult, blLen := bits(csd, 73, 62), bits(csd, 49, 47), bits(csd, 83, 80)
		return (cSize + 1) << (mult + 2) << blLen / BlockSize, nil
	case 1: // CSD 2.0 (SDHC/SDXC): (C_SIZE+1) * 512 KB
		return (bits(csd, 69, 48) + 1) * 1024, nil
	}
	return 0, fmt.Errorf("sdhci: unknown CSD structure %d", bits(csd, 127, 126))
}

// transfer reads or writes p from block lba on, maxBlocks at a time.
func (h *host) transfer(ctx context.Context, lba uint64, p []byte, write bool) error {
	for len(p) > 0 {
		n := min(len(p)/BlockSize, maxBlocks)
		if err := h.chunk(ctx, lba, p[:n*BlockSize], write); err != nil {
			return err
		}
		p, lba = p[n*BlockSize:], lba+uint64(n)
	}
	return nil
}

func (h *host) chunk(ctx context.Context, lba uint64, p []byte, write bool) error {
	n := len(p) / BlockSize
	addr := uint32(lba)
	if !h.sdhc {
		addr = uint32(lba * BlockSize)
	}
	idx, mode := 17, uint32(0) // READ_SINGLE_BLOCK
	if write {
		idx = 24
	} else {
		mode |= xferRead
	}
	if n > 1 {
		idx++ // 18 / 25: multiple blocks, stopped by auto CMD12
		mode |= xferMulti | xferBlockCount | xferAutoCMD12
	}
	h.r.Write32(BLOCK, uint32(n)<<16|BlockSize)
	if err := h.cmdStart(ctx, idx, addr, r1|dataPres, mode); err != nil {
		return err
	}
	if err := h.waitInt(ctx, intCmdDone, fmt.Sprintf("CMD%d", idx)); err != nil {
		return err
	}
	ready := uint32(intReadRdy)
	if write {
		ready = intWriteRdy
	}
	for b := 0; b < n; b++ {
		if err := h.waitInt(ctx, ready, "data buffer"); err != nil {
			return err
		}
		blk := p[b*BlockSize : (b+1)*BlockSize]
		for i := 0; i < BlockSize; i += 4 {
			if write {
				h.r.Write32(BUFFER, uint32(blk[i])|uint32(blk[i+1])<<8|uint32(blk[i+2])<<16|uint32(blk[i+3])<<24)
			} else {
				w := h.r.Read32(BUFFER)
				blk[i], blk[i+1], blk[i+2], blk[i+3] = byte(w), byte(w>>8), byte(w>>16), byte(w>>24)
			}
		}
	}
	return h.waitInt(ctx, intXferDone, "transfer complete")
}
