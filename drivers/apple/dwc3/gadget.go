package dwc3

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
	"godeep/kernel/netdev"
)

// Device mode: when the port controller says another computer is the
// host, the port becomes a USB Ethernet adapter (CDC ECM, which Linux and
// macOS drive without extra drivers), after m1n1's usb_dwc3.c gadget.
// High speed only. Frames go one per transfer; the host computer reaches
// godeep at whatever address netd gives it (with no DHCP server on the
// cable, the static fallback 10.0.2.15: give the host 10.0.2.2/24).

// Device-mode registers.
const (
	GEVNTADRLO  = 0xc400
	GEVNTADRHI  = 0xc404
	GEVNTSIZ    = 0xc408
	GEVNTCOUNT  = 0xc40c
	DCFG        = 0xc700
	DCTL        = 0xc704
	DEVTEN      = 0xc708
	DSTS        = 0xc70c
	DGCMDPAR    = 0xc710
	DGCMD       = 0xc714
	DALEPENA    = 0xc720
	DEPCMDPAR2  = 0xc800 // + 0x10 per endpoint
	DEPCMDPAR1  = 0xc804
	DEPCMDPAR0  = 0xc808
	DEPCMD      = 0xc80c
	GCTL_DEVICE = 2 << 12

	DCTL_RUN_STOP   = 1 << 31
	DCTL_CSFTRST    = 1 << 30
	DCFG_ADDR_MASK  = 0x7f << 3
	DCFG_SPEED_MASK = 7
	DSTS_HALTED     = 1 << 22
	DEVTEN_EVENTS   = 1<<0 | 1<<1 | 1<<2 // disconnect, reset, connect done
	CMD_ACT         = 1 << 10

	cmdSetEPConfig   = 0x01
	cmdSetXferRes    = 0x02
	cmdSetStall      = 0x04
	cmdClearStall    = 0x05
	cmdStartTransfer = 0x06
	cmdEndTransfer   = 0x08
	cmdStartConfig   = 0x09
	dgcmdScratchLo   = 0x04
	dgcmdScratchHi   = 0x05

	epControl, epBulk, epIntr = 0, 2, 3

	trbHWO, trbLST, trbISP, trbIOC = 1 << 0, 1 << 1, 1 << 10, 1 << 11
	trbNormal                      = 1 << 4
	trbSetup                       = 2 << 4
	trbStatus2                     = 3 << 4
	trbData                        = 5 << 4

	evXferComplete, evXferNotReady = 1, 3
	devtDisconnect, devtReset      = 0, 1
	devtConnectDone                = 2

	eventBufSize = 256 // 64 events
	ep0Buf       = 512
	frameBuf     = 2048
)

// Physical endpoint numbers: 2*address + direction.
const (
	pEP0Out = 0
	pEP0In  = 1
	pIntrIn = 3 // 0x81
	pBulkOt = 4 // 0x02
	pBulkIn = 5 // 0x82
)

// GadgetCompatible is the device-manager entry of the gadget's NIC.
const GadgetCompatible = "godeep,usb-gadget-ecm"

// Gadget is the Mac as a USB Ethernet adapter: a netdev.NIC.
type Gadget struct {
	mac      [6]byte
	rx       chan []byte
	tx       chan []byte
	down     chan struct{}
	linkUp   atomic.Bool
	state    atomic.Pointer[string]
	rxN, txN atomic.Uint64
	drops    atomic.Uint64
}

var _ netdev.NIC = (*Gadget)(nil)

func (g *Gadget) MAC() [6]byte { return g.mac }
func (g *Gadget) MTU() int     { return 1500 }

func (g *Gadget) Send(ctx context.Context, frame []byte) error {
	if !g.linkUp.Load() {
		return nil // no host listening: drop, like a cable with nothing on it
	}
	select {
	case g.tx <- append([]byte(nil), frame...):
		return nil
	case <-g.down:
		return netdev.ErrDown
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Gadget) Recv(ctx context.Context) ([]byte, error) {
	select {
	case f := <-g.rx:
		return f, nil
	case <-g.down:
		return nil, netdev.ErrDown
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *Gadget) Devices() []string {
	st := "starting"
	if s := g.state.Load(); s != nil {
		st = *s
	}
	return []string{fmt.Sprintf("%s, USB device mode (CDC ECM): %s; rx %d frames (%d dropped), tx %d frames",
		net.HardwareAddr(g.mac[:]), st, g.rxN.Load(), g.drops.Load(), g.txN.Load())}
}

func (g *Gadget) setState(s string) { g.state.Store(&s) }

// GadgetDriver publishes a gadget a port found (Config: *Gadget).
func GadgetDriver() dev.Driver {
	return dev.Driver{Compatible: GadgetCompatible, Run: func(ctx context.Context, d *dev.Device) error {
		g, ok := d.Config.(*Gadget)
		if !ok {
			return errors.New("gadget: no adapter")
		}
		d.Ready(g)
		<-ctx.Done()
		return nil
	}}
}

// ep is one physical endpoint: a single TRB and its buffer.
type ep struct {
	trb, buf *dma.Buffer
	busy     bool
	n        int // bytes requested by the running transfer
}

// gadget runs device mode on a core that's up (PRTCAP device).
type gadget struct {
	r        mmio.Regs
	g        *Gadget
	evs      *dma.Buffer
	evOff    int
	eps      [6]*ep
	allocs   []*dma.Buffer
	ep0State int
	ep0Data  []byte // what the data phase sends
	altData  int
	pending  []byte // a frame waiting for bulk IN
	strings  [4][]byte
}

// ep0 states (m1n1's).
const (
	sIdle = iota
	sSetup
	sDataSend
	sDataSendDone
	sDataRecvStatus
	sDataRecvStatusDone
	sSendStatus
	sSendStatusDone
)

func (x *gadget) alloc(a *dma.Allocator, n int) (*dma.Buffer, error) {
	b, err := a.Alloc(n, 64)
	if err == nil {
		x.allocs = append(x.allocs, b)
	}
	return b, err
}

func (x *gadget) free() {
	for _, b := range x.allocs {
		b.Free()
	}
}

func (x *gadget) cmd(n int, c, p0, p1, p2 uint32) error {
	o := uint64(0x10 * n)
	x.r.Write32(DEPCMDPAR0+o, p0)
	x.r.Write32(DEPCMDPAR1+o, p1)
	x.r.Write32(DEPCMDPAR2+o, p2)
	x.r.Write32(DEPCMD+o, c|CMD_ACT)
	deadline := time.Now().Add(10 * time.Millisecond)
	for x.r.Read32(DEPCMD+o)&CMD_ACT != 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("gadget: endpoint %d command %#x timed out", n, c)
		}
	}
	if x.r.Read32(DEPCMD+o)>>15&1 != 0 {
		return fmt.Errorf("gadget: endpoint %d command %#x failed", n, c)
	}
	return nil
}

func (x *gadget) configure(n, typ, mps int) error {
	p0 := uint32(typ)<<1 | uint32(mps)<<3
	if typ != epControl {
		p0 |= uint32(n) << 17 // FIFO number
	}
	p1 := uint32(1<<8 | 1<<10 | n<<25) // complete + not-ready events
	if err := x.cmd(n, cmdSetEPConfig, p0, p1, 0); err != nil {
		return err
	}
	return x.cmd(n, cmdSetXferRes, 1, 0, 0)
}

// start runs one TRB on endpoint n: kind, length.
func (x *gadget) start(n int, kind uint32, length int) error {
	e := x.eps[n]
	if e.busy {
		return nil
	}
	b := e.trb
	binary.LittleEndian.PutUint32(b.Bytes[0:], uint32(e.buf.Bus))
	binary.LittleEndian.PutUint32(b.Bytes[4:], uint32(e.buf.Bus>>32))
	binary.LittleEndian.PutUint32(b.Bytes[8:], uint32(length))
	b.Store32(12, trbHWO|trbLST|trbISP|trbIOC|kind)
	e.n = length
	if err := x.cmd(n, cmdStartTransfer, uint32(b.Bus>>32), uint32(b.Bus), 0); err != nil {
		return err
	}
	e.busy = true
	return nil
}

// done returns the bytes a completed transfer moved.
func (x *gadget) done(n int) int {
	e := x.eps[n]
	e.busy = false
	left := int(e.trb.Load32(8) & 0xffffff)
	return max(0, e.n-left)
}

func (x *gadget) stall() {
	x.cmd(pEP0Out, cmdSetStall, 0, 0, 0)
	x.ep0State = sIdle
}

// init starts device mode: soft reset of the device side, event buffer,
// endpoints, and run.
func (x *gadget) init(a *dma.Allocator) error {
	r := x.r
	r.Write32(DCTL, r.Read32(DCTL)|DCTL_CSFTRST)
	deadline := time.Now().Add(100 * time.Millisecond)
	for r.Read32(DCTL)&DCTL_CSFTRST != 0 {
		if time.Now().After(deadline) {
			return errors.New("gadget: device soft reset timed out")
		}
		time.Sleep(time.Millisecond)
	}
	mmio.Modify(r, DCFG, DCFG_SPEED_MASK|DCFG_ADDR_MASK, 0) // high speed, address 0
	scratch, err := x.alloc(a, 16384)
	if err != nil {
		return err
	}
	for _, c := range []struct{ cmd, par uint32 }{{dgcmdScratchLo, uint32(scratch.Bus)}, {dgcmdScratchHi, uint32(scratch.Bus >> 32)}} {
		r.Write32(DGCMDPAR, c.par)
		r.Write32(DGCMD, c.cmd|CMD_ACT)
		for i := 0; r.Read32(DGCMD)&CMD_ACT != 0; i++ {
			if i > 1e6 {
				return errors.New("gadget: scratchpad command timed out")
			}
		}
	}
	if x.evs, err = x.alloc(a, eventBufSize); err != nil {
		return err
	}
	r.Write32(GEVNTADRLO, uint32(x.evs.Bus))
	r.Write32(GEVNTADRHI, uint32(x.evs.Bus>>32))
	r.Write32(GEVNTSIZ, eventBufSize)
	r.Write32(GEVNTCOUNT, 0)
	r.Write32(DEVTEN, DEVTEN_EVENTS)
	for n, size := range map[int]int{pEP0Out: ep0Buf, pEP0In: ep0Buf, pIntrIn: 64, pBulkOt: frameBuf, pBulkIn: frameBuf} {
		e := &ep{}
		if e.trb, err = x.alloc(a, 16); err != nil {
			return err
		}
		if e.buf, err = x.alloc(a, size); err != nil {
			return err
		}
		x.eps[n] = e
	}
	if err := x.cmd(pEP0Out, cmdStartConfig, 0, 0, 0); err != nil {
		return err
	}
	for _, c := range []struct{ n, typ, mps int }{{pEP0Out, epControl, 64}, {pEP0In, epControl, 64},
		{pIntrIn, epIntr, 16}, {pBulkOt, epBulk, 512}, {pBulkIn, epBulk, 512}} {
		if err := x.configure(c.n, c.typ, c.mps); err != nil {
			return err
		}
	}
	x.strings = gadgetStrings(x.g.mac)
	r.Write32(DALEPENA, 1<<pEP0Out|1<<pEP0In)
	r.Write32(DCTL, r.Read32(DCTL)|DCTL_RUN_STOP)
	return nil
}

// stop disconnects from the host.
func (x *gadget) stop() {
	x.r.Write32(DEVTEN, 0)
	x.r.Write32(DALEPENA, 0)
	x.r.Write32(DCTL, x.r.Read32(DCTL)&^DCTL_RUN_STOP)
	for i := 0; x.r.Read32(DSTS)&DSTS_HALTED == 0 && i < 1e5; i++ {
	}
}

// poll handles every event in the buffer.
func (x *gadget) poll() {
	n := int(x.r.Read32(GEVNTCOUNT)&0xfffc) / 4
	for i := 0; i < n; i++ {
		e := x.evs.Load32(x.evOff)
		x.evOff = (x.evOff + 4) % eventBufSize
		x.event(e)
	}
	if n > 0 {
		x.r.Write32(GEVNTCOUNT, uint32(4*n))
	}
	x.kick()
}

func (x *gadget) event(e uint32) {
	if e&1 != 0 { // device event
		if e>>1&0x7f != 0 {
			return
		}
		switch e >> 8 & 0xf {
		case devtReset:
			for n := range x.eps {
				if x.eps[n] != nil {
					x.eps[n].busy = false
				}
			}
			mmio.Modify(x.r, DCFG, DCFG_ADDR_MASK, 0)
			x.r.Write32(DALEPENA, 1<<pEP0Out|1<<pEP0In)
			x.g.linkUp.Store(false)
			x.g.setState("reset by the host")
		case devtConnectDone:
			x.start(pEP0Out, trbSetup, 8)
			x.ep0State = sSetup
			x.g.setState("connected, enumerating")
		case devtDisconnect:
			x.g.linkUp.Store(false)
			x.g.setState("host disconnected")
		}
		return
	}
	n, kind := int(e>>1&0x1f), int(e>>6&0xf)
	if n >= len(x.eps) || x.eps[n] == nil {
		return
	}
	switch {
	case kind == evXferComplete && n <= pEP0In:
		got := x.done(n)
		x.ep0Done(got)
	case kind == evXferNotReady && n <= pEP0In:
		if !x.eps[n].busy {
			x.ep0NotReady()
		}
	case kind == evXferComplete && n == pBulkOt:
		got := x.done(n)
		f := append([]byte(nil), x.eps[n].buf.Bytes[:got]...)
		select {
		case x.g.rx <- f:
			x.g.rxN.Add(1)
		default:
			x.g.drops.Add(1)
		}
		x.start(pBulkOt, trbNormal, frameBuf)
	case kind == evXferComplete && n == pBulkIn:
		x.done(n)
		x.g.txN.Add(1)
	}
}

// kick starts a pending frame on bulk IN.
func (x *gadget) kick() {
	if x.altData != 1 || x.eps[pBulkIn].busy {
		return
	}
	if x.pending == nil {
		select {
		case f := <-x.g.tx:
			x.pending = f
		default:
			return
		}
	}
	e := x.eps[pBulkIn]
	n := copy(e.buf.Bytes, x.pending)
	if n%512 == 0 { // pad instead of a zero-length packet: one short byte is fine for ECM
		e.buf.Bytes[n] = 0
		n++
	}
	if x.start(pBulkIn, trbNormal, n) == nil {
		x.pending = nil
	}
}

func (x *gadget) ep0Done(got int) {
	switch x.ep0State {
	case sSetup:
		x.setup(x.eps[pEP0Out].buf.Bytes[:8])
	case sDataRecvStatusDone, sSendStatusDone:
		x.start(pEP0Out, trbSetup, 8)
		x.ep0State = sSetup
	case sDataSendDone:
		x.ep0State = sDataRecvStatus
	default:
		x.stall()
	}
}

func (x *gadget) ep0NotReady() {
	switch x.ep0State {
	case sIdle:
		x.start(pEP0Out, trbSetup, 8)
		x.ep0State = sSetup
	case sDataSend:
		copy(x.eps[pEP0In].buf.Bytes, x.ep0Data)
		x.start(pEP0In, trbData, len(x.ep0Data))
		x.ep0State = sDataSendDone
	case sDataRecvStatus:
		x.start(pEP0Out, trbStatus2, 0)
		x.ep0State = sDataRecvStatusDone
	case sSendStatus:
		x.start(pEP0In, trbStatus2, 0)
		x.ep0State = sSendStatusDone
	}
}

// send answers a control IN request with b (cut to the host's length).
func (x *gadget) send(b []byte, length uint16) {
	x.ep0Data = b[:min(len(b), int(length))]
	x.ep0State = sDataSend
}

func (x *gadget) setup(p []byte) {
	typ, req := p[0], p[1]
	val, idx, length := binary.LittleEndian.Uint16(p[2:]), binary.LittleEndian.Uint16(p[4:]), binary.LittleEndian.Uint16(p[6:])
	ok := func() { x.ep0State = sSendStatus }
	switch {
	case typ == 0x80 && req == 6: // GET_DESCRIPTOR
		switch val >> 8 {
		case 1:
			x.send(deviceDesc, length)
		case 2:
			x.send(configDesc(), length)
		case 3:
			if int(val&0xff) < len(x.strings) {
				x.send(x.strings[val&0xff], length)
			} else {
				x.stall()
			}
		case 6:
			x.send(qualifierDesc, length)
		default:
			x.stall()
		}
	case typ == 0x00 && req == 5: // SET_ADDRESS
		mmio.Modify(x.r, DCFG, DCFG_ADDR_MASK, uint32(val&0x7f)<<3)
		ok()
	case typ == 0x00 && req == 9: // SET_CONFIGURATION
		if val == 1 {
			x.r.Write32(DALEPENA, 1<<pEP0Out|1<<pEP0In|1<<pIntrIn|1<<pBulkOt|1<<pBulkIn)
			x.g.setState("configured")
		} else {
			x.r.Write32(DALEPENA, 1<<pEP0Out|1<<pEP0In)
			x.altData = 0
			x.g.linkUp.Store(false)
		}
		ok()
	case typ == 0x80 && req == 0, typ == 0x81 && req == 0, typ == 0x82 && req == 0: // GET_STATUS
		x.send([]byte{1, 0}, length)
	case typ == 0x81 && req == 0x0a: // GET_INTERFACE
		x.send([]byte{byte(x.altData * int(idx&1))}, length)
	case typ == 0x01 && req == 0x0b: // SET_INTERFACE
		if idx == 1 {
			x.altData = int(val)
			if val == 1 {
				x.start(pBulkOt, trbNormal, frameBuf)
				x.g.linkUp.Store(true)
				x.g.setState("link up")
			} else {
				x.g.linkUp.Store(false)
				x.g.setState("configured, data interface off")
			}
		}
		ok()
	case typ == 0x02 && req == 1: // CLEAR_FEATURE(ENDPOINT_HALT)
		n := int(idx&0xf)<<1 | int(idx>>7&1)
		if n < len(x.eps) && x.eps[n] != nil {
			x.cmd(n, cmdClearStall, 0, 0, 0)
		}
		ok()
	case typ == 0x21 && req == 0x43: // SET_ETHERNET_PACKET_FILTER
		ok()
	default:
		x.stall()
	}
}

// Descriptors: a CDC ECM function, high speed.
var deviceDesc = []byte{18, 1, 0x00, 0x02, 0x02, 0, 0, 64, 0x09, 0x12, 0x01, 0x00, 0x00, 0x01, 1, 2, 0, 1}

var qualifierDesc = []byte{10, 6, 0x00, 0x02, 0x02, 0, 0, 64, 1, 0}

func configDesc() []byte {
	b := []byte{
		9, 2, 0, 0, 2, 1, 0, 0xc0, 0, // config: 2 interfaces, self-powered
		9, 4, 0, 0, 1, 0x02, 0x06, 0, 0, // comm interface: CDC ECM
		5, 0x24, 0x00, 0x10, 0x01, // header
		5, 0x24, 0x06, 0, 1, // union: control 0, data 1
		13, 0x24, 0x0f, 3, 0, 0, 0, 0, 0xea, 0x05, 0, 0, 0, // Ethernet: MAC string 3, max segment 1514
		7, 5, 0x81, 3, 16, 0, 9, // interrupt IN (notifications; none sent)
		9, 4, 1, 0, 0, 0x0a, 0, 0, 0, // data interface, alt 0: no endpoints
		9, 4, 1, 1, 2, 0x0a, 0, 0, 0, // alt 1: bulk IN and OUT
		7, 5, 0x82, 2, 0x00, 0x02, 0,
		7, 5, 0x02, 2, 0x00, 0x02, 0,
	}
	binary.LittleEndian.PutUint16(b[2:], uint16(len(b)))
	return b
}

func usbString(s string) []byte {
	b := []byte{byte(2 + 2*len(s)), 3}
	for _, c := range s {
		b = append(b, byte(c), 0)
	}
	return b
}

// gadgetStrings: languages, manufacturer, product, and the host side's
// MAC address (ours with the last byte flipped).
func gadgetStrings(mac [6]byte) [4][]byte {
	host := mac
	host[5] ^= 1
	return [4][]byte{{4, 3, 0x09, 0x04}, usbString("godeep"), usbString("godeep USB Ethernet"),
		usbString(fmt.Sprintf("%02X%02X%02X%02X%02X%02X", host[0], host[1], host[2], host[3], host[4], host[5]))}
}

// runGadget drives device mode until ctx is done.
func runGadget(ctx context.Context, r mmio.Regs, a *dma.Allocator, g *Gadget) error {
	x := &gadget{r: r, g: g}
	defer x.free()
	if err := x.init(a); err != nil {
		return err
	}
	defer x.stop()
	g.setState("waiting for the host")
	t := time.NewTicker(time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			g.linkUp.Store(false)
			return nil
		case <-t.C:
			x.poll()
		}
	}
}
