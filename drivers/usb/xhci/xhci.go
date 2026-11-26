// Package xhci drives a USB 3 host controller (xHCI, PCI class 0c0330),
// polled: controller reset and start, command and event rings, port reset,
// device addressing, descriptors, configuration, and interrupt IN transfers
// for HID boot keyboards and mice, bulk transfers for USB mass storage
// (storage.go: Bulk-Only Transport, SCSI, read-only, published as a block
// device), and hubs (hub.go: devices behind full-speed hubs, any depth).
//
// QEMU emulates it (qemu-xhci, usb-kbd, usb-mouse). Apple Silicon's USB-C
// ports are DWC3 controllers in xHCI host mode, so the same driver serves
// them once the Apple-specific PHY and USB-C port controller are set up.
//
// One goroutine (the driver's) owns the controller: it enumerates, then
// polls the event ring and the ports every PollInterval.
package xhci

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"godeep/drivers/pci"
	"godeep/drivers/usb"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/input"
	"godeep/kernel/mmio"
)

// ErrStopped is returned by a USB disk whose controller has stopped (the
// cable was pulled, or the controller restarted).
var ErrStopped = errors.New("usb: controller stopped")

// Compatible is the PCI class string for xHCI.
const Compatible = "pciclass,0c0330"

// Capability registers.
const (
	CAPLENGTH  = 0x00 // byte 0; HCIVERSION in bytes 2-3
	HCSPARAMS1 = 0x04
	HCSPARAMS2 = 0x08
	HCCPARAMS1 = 0x10
	DBOFF      = 0x14
	RTSOFF     = 0x18
)

// Operational registers (from CAPLENGTH).
const (
	USBCMD  = 0x00
	USBSTS  = 0x04
	CRCR    = 0x18 // 64-bit
	DCBAAP  = 0x30 // 64-bit
	CONFIG  = 0x38
	PORTSC0 = 0x400 // + 0x10 per port

	CMD_RUN   = 1 << 0
	CMD_RESET = 1 << 1
	STS_HALT  = 1 << 0
	STS_HSE   = 1 << 2 // host system error
	STS_CNR   = 1 << 11
)

// PORTSC bits.
const (
	PORT_CCS = 1 << 0
	PORT_PED = 1 << 1 // write 1 disables the port
	PORT_PR  = 1 << 4
	PORT_PP  = 1 << 9
	PORT_CSC = 1 << 17
	PORT_PEC = 1 << 18
	PORT_WRC = 1 << 19
	PORT_OCC = 1 << 20
	PORT_PRC = 1 << 21
	PORT_PLC = 1 << 22
	PORT_CEC = 1 << 23

	portChanges = PORT_CSC | PORT_PEC | PORT_WRC | PORT_OCC | PORT_PRC | PORT_PLC | PORT_CEC
	portKeep    = PORT_PP | 7<<25 // power and wake enables: preserved on every write
)

// Interrupter 0 (from RTSOFF + 0x20).
const (
	IMAN   = 0x00
	ERSTSZ = 0x08
	ERSTBA = 0x10 // 64-bit
	ERDP   = 0x18 // 64-bit; bit 3 (EHB) write-1-to-clear
)

// TRB types and completion codes.
const (
	trbNormal      = 1
	trbSetup       = 2
	trbData        = 3
	trbStatus      = 4
	trbLink        = 6
	trbEnableSlot  = 9
	trbDisableSlot = 10
	trbAddress     = 11
	trbConfigureEP = 12
	trbEvaluate    = 13
	trbResetEP     = 14
	trbSetDequeue  = 16
	trbTransferEv  = 32
	trbCommandEv   = 33
	trbPortEv      = 34

	ccSuccess = 1
	ccShort   = 13
	ccStall   = 6

	trbCycle = 1 << 0
	trbTC    = 1 << 1 // link: toggle cycle
	trbISP   = 1 << 2
	trbIOC   = 1 << 5
	trbIDT   = 1 << 6
	trbDirIn = 1 << 16
)

const (
	ringTRBs     = 64
	maxSlots     = 8
	PollInterval = 5 * time.Millisecond
	cmdTimeout   = 2 * time.Second
)

// Driver returns the xHCI driver for PCI controllers. keys receives typed
// bytes from USB keyboards (nil: keys only as events); regs maps BAR0.
func Driver(regs func(base uint64) mmio.Regs, keys chan<- byte) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		f, ok := d.Config.(*pci.Function)
		if !ok || f.BARs[0].Size == 0 {
			return errors.New("xhci: no PCI function with a BAR0")
		}
		if d.DMA == nil {
			return errors.New("xhci: the board has no DMA region")
		}
		a := d.DMA
		if f.DMA != nil { // behind an IOMMU
			a = f.DMA
		}
		return Run(ctx, d, Config{Regs: regs(f.BARs[0].CPU), DMA: a, Keys: keys})
	}}
}

// Config is a controller instance for Run.
type Config struct {
	Regs mmio.Regs      // from the capability registers on
	DMA  *dma.Allocator // device-visible memory: buffers' Bus addresses go to the controller
	Keys chan<- byte    // typed bytes from USB keyboards (nil: events only)
	// Names keeps the names of what the controller publishes (usbdisk0,
	// usbnet0) across runs; nil: a fresh set.
	Names *Names
	// Ready publishes the service (nil: d.Ready). A glue driver that runs
	// the controller again and again (DWC3) keeps its own service.
	Ready func(*Service)
}

// Run drives one controller until ctx is done (it then halts it) or it
// fails. Every DMA buffer is freed and the NICs it published report
// netdev.ErrDown when it returns.
func Run(ctx context.Context, d *dev.Device, c Config) error {
	if c.Names == nil {
		c.Names = &Names{}
	}
	h := &hc{r: c.Regs, dma: c.DMA, keys: c.Keys, svc: &Service{}, names: c.Names,
		reads: make(chan *readReq), sends: make(chan *sendReq), down: make(chan struct{})}
	if d != nil {
		h.publish, h.remove = d.Replace, d.Remove
	}
	defer h.free()
	defer close(h.down) // disks: ErrStopped
	defer func() {      // USB NICs stop with the controller (netd sees ErrDown)
		for _, n := range h.nics {
			n.stop()
		}
		for _, k := range h.disks { // their names go to the next run's devices
			h.names.release(k.name)
		}
		for _, n := range h.nics {
			h.names.release(n.name)
		}
	}()
	if err := h.init(ctx); err != nil {
		return err
	}
	switch {
	case c.Ready != nil:
		c.Ready(h.svc)
	case d != nil:
		d.Ready(h.svc)
	}
	h.scanPorts(ctx)
	t := time.NewTicker(PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			h.r.Write32(h.op+USBCMD, 0)
			return nil
		case now := <-t.C:
			if err := h.poll(ctx, now); err != nil {
				return err
			}
		case r := <-h.sends:
			r.done <- r.nic.send(ctx, r.frame)
		case r := <-h.reads:
			r.done <- r.disk.transfer(ctx, r.lba, r.p, r.write)
		}
	}
}

// Names hands out device-manager names for what controllers publish
// ("usbdisk0", "usbnet0"): unique across controllers that share it (or
// whose names collide: the next free number is taken). A name stays taken
// while its device is plugged in; when a controller runs anew, or a device
// comes back, it gets the lowest name it had before, so whoever follows a
// name (netd) finds the device a replugged cable brought back.
type Names struct {
	mu    sync.Mutex
	owned map[string][]string // by prefix, in order of first use
	inUse map[string]bool
}

// publish publishes node under the lowest free name prefix<N>.
func (n *Names) publish(add func(dev.Node) bool, prefix string, node dev.Node) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.owned == nil {
		n.owned, n.inUse = map[string][]string{}, map[string]bool{}
	}
	for _, name := range n.owned[prefix] {
		if !n.inUse[name] {
			node.Name = name
			if add != nil {
				add(node)
			}
			n.inUse[name] = true
			return name
		}
	}
	for i := 0; ; i++ {
		node.Name = fmt.Sprintf("%s%d", prefix, i)
		if add == nil || add(node) {
			n.owned[prefix] = append(n.owned[prefix], node.Name)
			n.inUse[node.Name] = true
			return node.Name
		}
	}
}

// release frees a name for the next device.
func (n *Names) release(name string) {
	n.mu.Lock()
	delete(n.inUse, name)
	n.mu.Unlock()
}

// Service is the driver's service: lsdev lines and the HID devices'
// input sources.
type Service struct {
	mu            sync.Mutex
	lines         []string
	sources       []input.Source
	keys, dropped int
	kbds          []*usb.BootKeyboard
	layout        *input.Layout
}

func (s *Service) Devices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string{}, s.lines...)
	if s.keys > 0 || s.dropped > 0 {
		out = append(out, fmt.Sprintf("%d keys typed (%d dropped)", s.keys, s.dropped))
	}
	return out
}

// SetLayout implements input.LayoutSetter for every USB keyboard, and the
// ones plugged in later.
func (s *Service) SetLayout(l *input.Layout) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.layout = l
	for _, k := range s.kbds {
		k.SetLayout(l)
	}
}

func (s *Service) InputSources() []input.Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]input.Source{}, s.sources...)
}

func (s *Service) addLine(l string) {
	s.mu.Lock()
	s.lines = append(s.lines, l)
	s.mu.Unlock()
}

// ring is a producer ring (command or transfer): TRBs, the last a link back.
type ring struct {
	buf *dma.Buffer
	enq int
	pcs uint32
}

func newRing(a *dma.Allocator) (*ring, error) {
	b, err := a.Alloc(ringTRBs*16, 64)
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint64(b.Bytes[(ringTRBs-1)*16:], b.Bus)
	return &ring{buf: b, pcs: 1}, nil
}

// push writes a TRB (the cycle bit last, with release ordering) and returns
// its address.
func (r *ring) push(param uint64, status, control uint32) uint64 {
	off := r.enq * 16
	binary.LittleEndian.PutUint64(r.buf.Bytes[off:], param)
	binary.LittleEndian.PutUint32(r.buf.Bytes[off+8:], status)
	r.buf.Store32(off+12, control&^trbCycle|r.pcs)
	addr := r.buf.Bus + uint64(off)
	r.enq++
	if r.enq == ringTRBs-1 { // hand the link TRB over, then wrap
		r.buf.Store32((ringTRBs-1)*16+12, trbLink<<10|trbTC|r.pcs)
		r.enq, r.pcs = 0, r.pcs^1
	}
	return addr
}

// event is a TRB from the event ring.
type event struct {
	param       uint64
	status, ctl uint32
}

func (e event) typ() int      { return int(e.ctl >> 10 & 0x3f) }
func (e event) code() int     { return int(e.status >> 24) }
func (e event) slot() int     { return int(e.ctl >> 24) }
func (e event) ep() int       { return int(e.ctl >> 16 & 0x1f) }
func (e event) residual() int { return int(e.status & 0xffffff) }

// endpoint is a configured interrupt IN endpoint of a HID device.
type endpoint struct {
	slot, dci int
	ring      *ring
	buf       *dma.Buffer
	size      int
	pending   bool
	handle    func([]byte, time.Time)
}

// device is an addressed USB device.
type device struct {
	slot, port int // port: the root port it hangs off (through hubs too)
	route      uint32
	where      string // "port 3" or "port 3.1" (behind a hub)
	speed      usb.Speed
	in, out    *dma.Buffer // input and output (device) contexts
	ep0        *ring
	mps0       int
	desc       usb.DeviceDesc
	tt         tt // the high-speed hub's transaction translator a low/full-speed device is behind

	// What the device brought, so an unplug takes it away again.
	hub     *hub
	names   []string // published device-manager entries
	sources []input.Source
	kbds    []*usb.BootKeyboard
}

// tt names a transaction translator: the slot of the high-speed hub that
// has it and the hub port the low- or full-speed device's tree hangs off.
// Zero for devices that talk to the controller at their own speed.
type tt struct{ hubSlot, port int }

// slotWord is the slot context's third word: TT hub slot ID, TT port.
func (t tt) slotWord() uint32 { return uint32(t.hubSlot&0xff) | uint32(t.port&0xff)<<8 }

// childTT is the translator for a device at speed on hub port p of d.
func (d *device) childTT(p int, speed usb.Speed) tt {
	switch {
	case speed != usb.Low && speed != usb.Full:
		return tt{}
	case d.speed == usb.High:
		return tt{hubSlot: d.slot, port: p} // this hub translates
	}
	return d.tt // a full-speed hub behind a high-speed one: its translator
}

type hc struct {
	r           mmio.Regs
	dma         *dma.Allocator
	keys        chan<- byte
	svc         *Service
	op, rt, db  uint64
	ports       int
	ctxSize     int
	dcbaa       *dma.Buffer
	scratch     []*dma.Buffer
	cmd         *ring
	evSeg, erst *dma.Buffer
	evDeq       int
	evCCS       uint32
	devs        map[int]*device // by port
	eps         []*endpoint
	queued      []event // events read while waiting for something else
	kbds        []*usb.BootKeyboard
	allocs      []*dma.Buffer
	hubs        []*hub
	disks       []*Disk
	publish     func(dev.Node) bool // publishes discovered devices (disks, NICs): dev.Device.Replace
	remove      func(string) bool   // forgets an unplugged one: dev.Device.Remove
	names       *Names
	down        chan struct{} // closed when Run returns
	reads       chan *readReq // mass-storage reads, served by this goroutine
	sends       chan *sendReq // USB Ethernet frames out
	nics        []*NIC
}

func (h *hc) alloc(size, align int) (*dma.Buffer, error) {
	b, err := h.dma.Alloc(size, align)
	if err == nil {
		h.allocs = append(h.allocs, b)
	}
	return b, err
}

func (h *hc) free() {
	for _, b := range h.allocs {
		b.Free()
	}
	h.allocs = nil
}

func (h *hc) write64(off, v uint64) {
	h.r.Write32(off, uint32(v))
	h.r.Write32(off+4, uint32(v>>32))
}

func (h *hc) waitOp(ctx context.Context, reg uint64, mask, want uint32, what string) error {
	deadline := time.Now().Add(time.Second)
	for h.r.Read32(h.op+reg)&mask != want {
		if time.Now().After(deadline) {
			return fmt.Errorf("xhci: timeout waiting for %s", what)
		}
		if err := sleep(ctx, time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func (h *hc) init(ctx context.Context) error {
	h.op = uint64(h.r.Read32(CAPLENGTH) & 0xff)
	sp1, sp2, cp1 := h.r.Read32(HCSPARAMS1), h.r.Read32(HCSPARAMS2), h.r.Read32(HCCPARAMS1)
	h.db, h.rt = uint64(h.r.Read32(DBOFF)&^3), uint64(h.r.Read32(RTSOFF)&^0x1f)
	h.ports = int(sp1 >> 24)
	slots := min(int(sp1&0xff), maxSlots)
	h.ctxSize = 32
	if cp1&(1<<2) != 0 {
		h.ctxSize = 64
	}
	if cp1&1 == 0 {
		return errors.New("xhci: controller can't address 64-bit memory")
	}
	h.devs = map[int]*device{}

	if err := h.waitOp(ctx, USBSTS, STS_CNR, 0, "controller ready"); err != nil {
		return err
	}
	h.r.Write32(h.op+USBCMD, 0)
	if err := h.waitOp(ctx, USBSTS, STS_HALT, STS_HALT, "halt"); err != nil {
		return err
	}
	h.r.Write32(h.op+USBCMD, CMD_RESET)
	if err := h.waitOp(ctx, USBCMD, CMD_RESET, 0, "reset"); err != nil {
		return err
	}
	if err := h.waitOp(ctx, USBSTS, STS_CNR, 0, "controller ready after reset"); err != nil {
		return err
	}
	h.r.Write32(h.op+CONFIG, uint32(slots))

	var err error
	if h.dcbaa, err = h.alloc((maxSlots+1)*8, 64); err != nil {
		return err
	}
	if n := int(sp2>>27&0x1f | sp2>>21&0x1f<<5); n > 0 { // scratchpad buffers
		arr, err := h.alloc(n*8, 64)
		if err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			p, err := h.alloc(4096, 4096)
			if err != nil {
				return err
			}
			binary.LittleEndian.PutUint64(arr.Bytes[8*i:], p.Bus)
		}
		binary.LittleEndian.PutUint64(h.dcbaa.Bytes, arr.Bus)
	}
	h.write64(h.op+DCBAAP, h.dcbaa.Bus)

	if h.cmd, err = newRing(h.dma); err != nil {
		return err
	}
	h.allocs = append(h.allocs, h.cmd.buf)
	h.write64(h.op+CRCR, h.cmd.buf.Bus|1)

	if h.evSeg, err = h.alloc(ringTRBs*16, 64); err != nil {
		return err
	}
	if h.erst, err = h.alloc(16, 64); err != nil {
		return err
	}
	binary.LittleEndian.PutUint64(h.erst.Bytes, h.evSeg.Bus)
	binary.LittleEndian.PutUint32(h.erst.Bytes[8:], ringTRBs)
	h.evCCS = 1
	ir := h.rt + 0x20
	h.r.Write32(ir+ERSTSZ, 1)
	h.write64(ir+ERDP, h.evSeg.Bus)
	h.write64(ir+ERSTBA, h.erst.Bus)
	h.r.Write32(ir+IMAN, 1) // polled: interrupts off, pending flag cleared

	h.r.Write32(h.op+USBCMD, CMD_RUN)
	if err := h.waitOp(ctx, USBSTS, STS_HALT, 0, "run"); err != nil {
		return err
	}
	v := h.r.Read32(CAPLENGTH) >> 16
	h.svc.addLine(fmt.Sprintf("xHCI %x.%02x, %d ports, %d slots", v>>8, v&0xff, h.ports, slots))
	return nil
}

// nextEvent returns the next event TRB, if any, and moves the dequeue
// pointer on.
func (h *hc) nextEvent() (event, bool) {
	off := h.evDeq * 16
	ctl := h.evSeg.Load32(off + 12) // acquire: the cycle bit says the TRB is complete
	if ctl&1 != h.evCCS {
		return event{}, false
	}
	e := event{param: uint64(h.evSeg.Load32(off)) | uint64(h.evSeg.Load32(off+4))<<32,
		status: h.evSeg.Load32(off + 8), ctl: ctl}
	h.evDeq++
	if h.evDeq == ringTRBs {
		h.evDeq, h.evCCS = 0, h.evCCS^1
	}
	h.write64(h.rt+0x20+ERDP, h.evSeg.Bus+uint64(h.evDeq*16)|1<<3)
	return e, true
}

// wait polls the event ring until match accepts an event; others are
// queued for the main loop.
func (h *hc) wait(ctx context.Context, what string, match func(event) bool) (event, error) {
	deadline := time.Now().Add(cmdTimeout)
	for spins := 0; ; spins++ {
		if e, ok := h.nextEvent(); ok {
			if match(e) {
				return e, nil
			}
			h.queued = append(h.queued, e)
			continue
		}
		if time.Now().After(deadline) {
			return event{}, fmt.Errorf("xhci: timeout waiting for %s", what)
		}
		if spins > 100 {
			if err := sleep(ctx, 100*time.Microsecond); err != nil {
				return event{}, err
			}
		}
	}
}

// command runs a command and returns its completion's slot ID.
func (h *hc) command(ctx context.Context, param uint64, status, ctl uint32) (int, error) {
	addr := h.cmd.push(param, status, ctl)
	h.r.Write32(h.db, 0)
	e, err := h.wait(ctx, "a command", func(e event) bool { return e.typ() == trbCommandEv && e.param == addr })
	if err != nil {
		return 0, err
	}
	if e.code() != ccSuccess {
		return 0, fmt.Errorf("xhci: command %d failed with code %d", ctl>>10&0x3f, e.code())
	}
	return e.slot(), nil
}

func (h *hc) portsc(port int) uint64 { return h.op + PORTSC0 + 0x10*uint64(port-1) }

// scanPorts enumerates devices on ports that have one and none yet.
func (h *hc) scanPorts(ctx context.Context) {
	for port := 1; port <= h.ports; port++ {
		v := h.r.Read32(h.portsc(port))
		if v&portChanges != 0 { // acknowledge, preserving everything else
			h.r.Write32(h.portsc(port), v&portKeep|v&portChanges)
		}
		if v&PORT_CCS == 0 {
			if _, ok := h.devs[port]; ok {
				h.detach(ctx, h.devs[port])
				delete(h.devs, port)
				h.svc.addLine(fmt.Sprintf("port %d: disconnected", port))
			}
			continue
		}
		if _, ok := h.devs[port]; ok {
			continue
		}
		if err := h.attach(ctx, port); err != nil {
			h.devs[port] = &device{port: port} // don't retry every poll
			h.svc.addLine(fmt.Sprintf("port %d: %v", port, err))
		}
	}
}

func (h *hc) poll(ctx context.Context, now time.Time) error {
	if h.r.Read32(h.op+USBSTS)&STS_HSE != 0 {
		return errors.New("xhci: host system error")
	}
	for {
		e, ok := h.nextEvent()
		if !ok {
			break
		}
		h.queued = append(h.queued, e)
	}
	evs := h.queued
	h.queued = nil
	rescan := false
	for _, e := range evs {
		switch e.typ() {
		case trbPortEv:
			rescan = true
		case trbTransferEv:
			for _, ep := range h.eps {
				if ep.slot == e.slot() && ep.dci == e.ep() && ep.pending {
					ep.pending = false
					if c := e.code(); c == ccSuccess || c == ccShort {
						n := max(0, ep.size-e.residual())
						ep.handle(ep.buf.Bytes[:n], now)
					}
				}
			}
		}
	}
	for _, ep := range h.eps {
		if !ep.pending {
			h.queueIn(ep)
		}
	}
	for _, k := range h.kbds {
		k.Tick(now)
	}
	if rescan {
		h.scanPorts(ctx)
	}
	for _, hb := range h.hubs {
		if hb.changed {
			hb.changed = false
			hb.scan(ctx)
		}
	}
	return nil
}

func (h *hc) queueIn(ep *endpoint) {
	ep.ring.push(ep.buf.Bus, uint32(ep.size), trbNormal<<10|trbIOC|trbISP)
	ep.pending = true
	h.r.Write32(h.db+4*uint64(ep.slot), uint32(ep.dci))
}

// resetPort resets a USB 2 port (USB 3 ports enable themselves) and
// returns its speed.
func (h *hc) resetPort(ctx context.Context, port int) (usb.Speed, error) {
	reg := h.portsc(port)
	if v := h.r.Read32(reg); v&PORT_PED == 0 {
		h.r.Write32(reg, v&portKeep|PORT_PR)
		deadline := time.Now().Add(time.Second)
		for {
			v = h.r.Read32(reg)
			if v&PORT_PRC != 0 && v&PORT_PR == 0 {
				break
			}
			if time.Now().After(deadline) {
				return 0, errors.New("port reset timed out")
			}
			if err := sleep(ctx, time.Millisecond); err != nil {
				return 0, err
			}
		}
		h.r.Write32(reg, v&portKeep|v&portChanges)
	}
	v := h.r.Read32(reg)
	if v&PORT_PED == 0 {
		return 0, errors.New("port not enabled after reset")
	}
	return usb.Speed(v >> 10 & 0xf), nil
}

// context offsets inside an input context (the input control context
// comes first) and a device context.
func (h *hc) inCtx(i int) int  { return (i + 1) * h.ctxSize } // 0 slot, 1 EP0, ...
func (h *hc) outCtx(i int) int { return i * h.ctxSize }

func put32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }

func (h *hc) attach(ctx context.Context, port int) error {
	speed, err := h.resetPort(ctx, port)
	if err != nil {
		return err
	}
	d, err := h.enumerate(ctx, port, 0, fmt.Sprintf("port %d", port), speed, tt{})
	if d != nil {
		h.devs[port] = d
	}
	return err
}

// enumerate addresses and configures a device at a root port, or behind
// hubs (route: one 4-bit hub port number per tier), and binds drivers to
// its interfaces. The device is returned even on a later error, so the
// caller doesn't retry it every poll.
func (h *hc) enumerate(ctx context.Context, port int, route uint32, where string, speed usb.Speed, t tt) (*device, error) {
	slot, err := h.command(ctx, 0, 0, trbEnableSlot<<10)
	if err != nil {
		return nil, err
	}
	d := &device{slot: slot, port: port, route: route, where: where, speed: speed, mps0: speed.DefaultMaxPacket0(), tt: t}
	if d.out, err = h.alloc(32*h.ctxSize, 64); err != nil {
		return d, err
	}
	if d.in, err = h.alloc(33*h.ctxSize, 64); err != nil {
		return d, err
	}
	if d.ep0, err = newRing(h.dma); err != nil {
		return d, err
	}
	h.allocs = append(h.allocs, d.ep0.buf)
	binary.LittleEndian.PutUint64(h.dcbaa.Bytes[8*slot:], d.out.Bus)

	in := d.in.Bytes
	put32(in, 4, 1<<0|1<<1) // add slot and EP0
	put32(in, h.inCtx(0), route&0xfffff|uint32(speed)<<20|1<<27)
	put32(in, h.inCtx(0)+4, uint32(port)<<16)
	if speed == usb.Low || speed == usb.Full {
		put32(in, h.inCtx(0)+8, t.slotWord())
	}
	h.ep0Ctx(d)
	if _, err := h.command(ctx, d.in.Bus, 0, trbAddress<<10|uint32(slot)<<24); err != nil {
		return d, fmt.Errorf("address device: %w", err)
	}

	buf, err := h.alloc(512, 64)
	if err != nil {
		return d, err
	}
	b, err := h.control(ctx, d, usb.GetDescriptor(usb.DescDevice, 0, 8), buf)
	if err != nil {
		return d, fmt.Errorf("device descriptor: %w", err)
	}
	dd, err := usb.ParseDevice(b)
	if err != nil {
		return d, err
	}
	mps0 := dd.MaxPacket0
	if speed == usb.Super && mps0 > 0 && mps0 < 16 { // SuperSpeed: an exponent (9 = 512)
		mps0 = 1 << mps0
	}
	if mps0 != d.mps0 && mps0 > 0 { // evaluate context with the real size
		d.mps0 = mps0
		clear(in)
		put32(in, 4, 1<<1)
		h.ep0Ctx(d)
		if _, err := h.command(ctx, d.in.Bus, 0, trbEvaluate<<10|uint32(slot)<<24); err != nil {
			return d, fmt.Errorf("evaluate context: %w", err)
		}
	}
	if b, err = h.control(ctx, d, usb.GetDescriptor(usb.DescDevice, 0, 18), buf); err != nil {
		return d, fmt.Errorf("device descriptor: %w", err)
	}
	if d.desc, err = usb.ParseDevice(b); err != nil {
		return d, err
	}
	// Several configurations (QEMU's usb-net: RNDIS, then CDC Ethernet):
	// take the first with an interface we drive, else the first.
	var cfg usb.Config
	for idx := 0; idx < max(1, d.desc.Configs); idx++ {
		c, err := h.config(ctx, d, idx, buf)
		if err != nil {
			if idx == 0 {
				return d, err
			}
			break
		}
		if idx == 0 {
			cfg = c
		}
		if supported(c) {
			cfg = c
			break
		}
	}
	if _, err := h.control(ctx, d, usb.Setup{Request: usb.ReqSetConfiguration, Value: uint16(cfg.Value)}, nil); err != nil {
		return d, fmt.Errorf("set configuration: %w", err)
	}

	var kinds []string
	for _, it := range cfg.Interfaces {
		if it.Alt != 0 {
			continue
		}
		switch {
		case it.Class == usb.ClassHub:
			if err := h.newHub(ctx, d, it); err != nil {
				return d, fmt.Errorf("hub: %w", err)
			}
			kinds = append(kinds, "hub")
			continue
		case it.Class == usb.ClassComm && it.Subclass == usb.SubclassECM:
			if err := h.newECM(ctx, d, cfg, it); err != nil {
				return d, fmt.Errorf("ethernet: %w", err)
			}
			kinds = append(kinds, "ethernet")
			continue
		case it.Class == usb.ClassComm && it.Subclass == usb.SubclassNCM:
			if err := h.newNCM(ctx, d, cfg, it); err != nil {
				return d, fmt.Errorf("ethernet (NCM): %w", err)
			}
			kinds = append(kinds, "ethernet")
			continue
		case it.Class == usb.ClassMassStorage && it.Subclass == 6 && it.Protocol == 0x50:
			if err := h.newDisk(ctx, d, it); err != nil {
				return d, fmt.Errorf("mass storage: %w", err)
			}
			kinds = append(kinds, "mass storage")
			continue
		}
		kind := it.BootKind()
		if kind == "" {
			continue
		}
		for _, e := range it.Endpoints {
			if !e.IsInterruptIn() {
				continue
			}
			if err := h.hidEndpoint(ctx, d, it, e, kind); err != nil {
				return d, fmt.Errorf("%s: %w", kind, err)
			}
			kinds = append(kinds, kind)
			break
		}
	}
	what := "no driver for its interfaces"
	if len(kinds) > 0 {
		what = strings.Join(kinds, ", ")
		if !strings.Contains(what, "hub") && !strings.Contains(what, "mass") && !strings.Contains(what, "ethernet") {
			what = "HID boot " + what
		}
	}
	h.svc.addLine(fmt.Sprintf("%s: %04x:%04x, %s, slot %d: %s", where, d.desc.Vendor, d.desc.Product, speed, slot, what))
	return d, nil
}

func (h *hc) ep0Ctx(d *device) {
	c := h.inCtx(1)
	in := d.in.Bytes
	put32(in, c+4, 3<<1|4<<3|uint32(d.mps0)<<16) // CErr 3, control endpoint
	binary.LittleEndian.PutUint64(in[c+8:], d.ep0.buf.Bus+uint64(d.ep0.enq*16)|uint64(d.ep0.pcs))
	put32(in, c+16, 8) // average TRB length
}

// control runs a control transfer on EP0 and returns the bytes read.
func (h *hc) control(ctx context.Context, d *device, s usb.Setup, buf *dma.Buffer) ([]byte, error) {
	setup := uint64(s.RequestType) | uint64(s.Request)<<8 | uint64(s.Value)<<16 | uint64(s.Index)<<32 | uint64(s.Length)<<48
	in := s.RequestType&0x80 != 0
	trt := uint32(0)
	if s.Length > 0 {
		trt = 2
		if in {
			trt = 3
		}
	}
	d.ep0.push(setup, 8, trbSetup<<10|trbIDT|trt<<16)
	var dataTRB uint64
	if s.Length > 0 {
		dir := uint32(0)
		if in {
			dir = trbDirIn
		}
		dataTRB = d.ep0.push(buf.Bus, uint32(s.Length), trbData<<10|dir|trbISP|trbIOC)
	}
	statusDir := uint32(trbDirIn)
	if s.Length > 0 && in {
		statusDir = 0
	}
	status := d.ep0.push(0, 0, trbStatus<<10|statusDir|trbIOC)
	h.r.Write32(h.db+4*uint64(d.slot), 1)

	n := int(s.Length)
	if dataTRB != 0 {
		e, err := h.wait(ctx, "control data", func(e event) bool {
			return e.typ() == trbTransferEv && e.slot() == d.slot && e.ep() == 1 && (e.param == dataTRB || e.param == status)
		})
		if err != nil {
			return nil, err
		}
		if c := e.code(); c != ccSuccess && c != ccShort {
			return nil, fmt.Errorf("transfer failed with code %d", c)
		}
		if e.param == dataTRB {
			n -= e.residual()
		} else {
			return buf.Bytes[:n], nil // the status stage completed first: nothing short
		}
	}
	e, err := h.wait(ctx, "control status", func(e event) bool {
		return e.typ() == trbTransferEv && e.slot() == d.slot && e.ep() == 1 && e.param == status
	})
	if err != nil {
		return nil, err
	}
	if c := e.code(); c != ccSuccess {
		return nil, fmt.Errorf("status stage failed with code %d", c)
	}
	if buf == nil {
		return nil, nil
	}
	return buf.Bytes[:max(0, n)], nil
}

// Endpoint types in an endpoint context.
const (
	epBulkOut     = 2
	epBulkIn      = 6
	epInterruptIn = 7
	slotHub       = 1 << 26
)

// config reads configuration descriptor idx.
func (h *hc) config(ctx context.Context, d *device, idx int, buf *dma.Buffer) (usb.Config, error) {
	b, err := h.control(ctx, d, usb.GetDescriptor(usb.DescConfig, byte(idx), 9), buf)
	if err != nil || len(b) < 4 {
		return usb.Config{}, fmt.Errorf("configuration descriptor: %v", err)
	}
	total := min(int(binary.LittleEndian.Uint16(b[2:])), len(buf.Bytes))
	if b, err = h.control(ctx, d, usb.GetDescriptor(usb.DescConfig, byte(idx), total), buf); err != nil {
		return usb.Config{}, fmt.Errorf("configuration descriptor: %w", err)
	}
	return usb.ParseConfig(b)
}

// supported reports a configuration with an interface this driver drives.
func supported(c usb.Config) bool {
	for _, it := range c.Interfaces {
		switch {
		case it.Class == usb.ClassHub, it.Class == usb.ClassComm && (it.Subclass == usb.SubclassECM || it.Subclass == usb.SubclassNCM),
			it.Class == usb.ClassMassStorage, it.BootKind() != "":
			return true
		}
	}
	return false
}

// configureEP adds endpoint e (of type typ) to the device and returns its
// device context index and transfer ring. slotExtra is ORed into the slot
// context's first three words (a hub's flag, port count and TT think time).
func (h *hc) configureEP(ctx context.Context, d *device, e usb.Endpoint, typ int, slotExtra [3]uint32) (int, *ring, error) {
	r, err := newRing(h.dma)
	if err != nil {
		return 0, nil, err
	}
	h.allocs = append(h.allocs, r.buf)
	dci := 2 * e.Num()
	if e.Address&0x80 != 0 {
		dci++
	}
	in := d.in.Bytes
	clear(in)
	put32(in, 4, 1<<0|1<<uint(dci))
	// slot context from the output context, with the new last endpoint
	copy(in[h.inCtx(0):h.inCtx(0)+h.ctxSize], d.out.Bytes[h.outCtx(0):h.outCtx(0)+h.ctxSize])
	sc := binary.LittleEndian.Uint32(in[h.inCtx(0):])
	entries := max(int(sc>>27), dci) // the last valid endpoint: only ever grows
	put32(in, h.inCtx(0), sc&^(0x1f<<27)|uint32(entries)<<27|slotExtra[0])
	put32(in, h.inCtx(0)+4, binary.LittleEndian.Uint32(in[h.inCtx(0)+4:])|slotExtra[1])
	put32(in, h.inCtx(0)+8, binary.LittleEndian.Uint32(in[h.inCtx(0)+8:])|slotExtra[2])
	c := h.inCtx(dci)
	if typ == epInterruptIn {
		put32(in, c, uint32(interval(d.speed, e.Interval))<<16)
	}
	put32(in, c+4, 3<<1|uint32(typ)<<3|uint32(e.MaxPacket)<<16) // CErr 3
	binary.LittleEndian.PutUint64(in[c+8:], r.buf.Bus|1)
	avg := uint32(e.MaxPacket)
	if typ != epInterruptIn {
		avg = 3072 // bulk: the spec's suggested average TRB length
	}
	put32(in, c+16, uint32(e.MaxPacket)<<16|avg) // max ESIT payload, average TRB length
	if _, err := h.command(ctx, d.in.Bus, 0, trbConfigureEP<<10|uint32(d.slot)<<24); err != nil {
		return 0, nil, fmt.Errorf("configure endpoint: %w", err)
	}
	return dci, r, nil
}

// interruptIn configures an interrupt IN endpoint (the caller sets its
// handler and starts it with queueIn).
func (h *hc) interruptIn(ctx context.Context, d *device, e usb.Endpoint, slotExtra ...[3]uint32) (*endpoint, error) {
	var extra [3]uint32
	if len(slotExtra) > 0 {
		extra = slotExtra[0]
	}
	dci, r, err := h.configureEP(ctx, d, e, epInterruptIn, extra)
	if err != nil {
		return nil, err
	}
	buf, err := h.alloc(max(e.MaxPacket, 8), 64)
	if err != nil {
		return nil, err
	}
	return &endpoint{slot: d.slot, dci: dci, ring: r, buf: buf, size: e.MaxPacket}, nil
}

// hidEndpoint configures an interrupt IN endpoint and starts reading it.
func (h *hc) hidEndpoint(ctx context.Context, d *device, it usb.Interface, e usb.Endpoint, kind string) error {
	if _, err := h.control(ctx, d, usb.Setup{RequestType: usb.ReqTypeClassIf, Request: usb.ReqSetProtocol, Index: uint16(it.Number)}, nil); err != nil {
		return fmt.Errorf("set boot protocol: %w", err)
	}
	if kind == "keyboard" { // reports only on change
		h.control(ctx, d, usb.Setup{RequestType: usb.ReqTypeClassIf, Request: usb.ReqSetIdle, Index: uint16(it.Number)}, nil)
	}
	ep, err := h.interruptIn(ctx, d, e)
	if err != nil {
		return err
	}
	events := &input.Broadcaster{Kind: kind}
	switch kind {
	case "keyboard":
		k := &usb.BootKeyboard{}
		k.Events = events
		k.Emit = h.emit
		h.kbds = append(h.kbds, k)
		d.kbds = append(d.kbds, k)
		h.svc.mu.Lock()
		if h.svc.layout != nil {
			k.SetLayout(h.svc.layout)
		}
		h.svc.kbds = append(h.svc.kbds, k)
		h.svc.mu.Unlock()
		ep.handle = k.Report
	default:
		m := &usb.BootMouse{Events: events}
		ep.handle = m.Report
	}
	h.eps = append(h.eps, ep)
	h.svc.mu.Lock()
	h.svc.sources = append(h.svc.sources, events)
	d.sources = append(d.sources, events)
	h.svc.mu.Unlock()
	h.queueIn(ep)
	return nil
}

// interval converts bInterval to the endpoint context's exponent (in
// 125 µs units): frames for low/full speed, already 2^(n-1) microframes
// for high and SuperSpeed.
func interval(s usb.Speed, b byte) int {
	if s == usb.High || s == usb.Super {
		return max(0, min(int(b)-1, 15))
	}
	n := 3 // 1 ms = 8 microframes
	for f := 2; f <= int(b) && n < 10; f *= 2 {
		n++
	}
	return n
}

func (h *hc) emit(b []byte) {
	sent, dropped := 0, 0
	for _, x := range b {
		if h.keys == nil {
			dropped++
			continue
		}
		select {
		case h.keys <- x:
			sent++
		default:
			dropped++
		}
	}
	h.svc.mu.Lock()
	h.svc.keys += sent
	h.svc.dropped += dropped
	h.svc.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// detach takes away everything an unplugged device brought (and, for a
// hub, everything behind it): its endpoints stop being polled, its NICs
// report netdev.ErrDown and its disks ErrStopped, their device-manager
// entries go and their names are free again, its keyboards and input
// sources leave the service, and its slot is disabled. (Its DMA buffers
// are kept until the controller stops.)
func (h *hc) detach(ctx context.Context, d *device) {
	if d == nil || d.slot == 0 {
		return
	}
	if d.hub != nil {
		for _, kid := range d.hub.kids {
			h.detach(ctx, kid)
		}
		h.hubs = without(h.hubs, d.hub)
	}
	keep := h.eps[:0]
	for _, ep := range h.eps {
		if ep.slot != d.slot {
			keep = append(keep, ep)
		}
	}
	h.eps = keep
	nics := h.nics[:0]
	for _, n := range h.nics {
		if n.d == d {
			n.stop()
		} else {
			nics = append(nics, n)
		}
	}
	h.nics = nics
	disks := h.disks[:0]
	for _, k := range h.disks {
		if k.d == d {
			k.stop()
		} else {
			disks = append(disks, k)
		}
	}
	h.disks = disks
	for _, name := range d.names {
		if h.remove != nil {
			h.remove(name)
		}
		h.names.release(name)
	}
	for _, k := range d.kbds {
		h.kbds = without(h.kbds, k)
	}
	h.svc.mu.Lock()
	for _, k := range d.kbds {
		h.svc.kbds = without(h.svc.kbds, k)
	}
	for _, src := range d.sources {
		h.svc.sources = without(h.svc.sources, src)
	}
	h.svc.mu.Unlock()
	if _, err := h.command(ctx, 0, 0, trbDisableSlot<<10|uint32(d.slot)<<24); err == nil {
		binary.LittleEndian.PutUint64(h.dcbaa.Bytes[8*d.slot:], 0)
	}
	d.slot = 0
}

func without[T comparable](s []T, x T) []T {
	out := s[:0]
	for _, v := range s {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}
