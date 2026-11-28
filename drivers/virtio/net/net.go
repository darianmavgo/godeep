// Package net drives a virtio network device (virtio device ID 1) over the
// virtio-mmio transport, or over legacy virtio-pci (Parallels Desktop:
// PCIDriver). It publishes a *NIC, an Ethernet frame device that
// net/netstack runs TCP/IP on.
//
// One goroutine (the driver's) owns the device and both queues: receive
// (queue 0, buffers the device fills) and transmit (queue 1). Send hands a
// frame to it over a channel; received frames go out on a buffered channel
// that Recv reads, and are dropped (counted) when nobody keeps up.
//
// Interrupts or polling: by default the driver waits on its interrupt line.
// With Options.Poll it polls instead (used under HVF, where QEMU 11.1.x's WFI
// bug makes device interrupts spin the idle vCPU), quickly while traffic
// flows and slowly when idle.
package net

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	stdnet "net"
	"sync/atomic"
	"time"

	"godeep/drivers/pci"
	vmmio "godeep/drivers/virtio/mmio"
	vpci "godeep/drivers/virtio/pci"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/mmio"
	"godeep/kernel/netdev"
)

// DeviceID is virtio-net's device ID.
const DeviceID = 1

// Feature bits.
const (
	FeatureMAC    = 1 << 5  // config holds the MAC address
	FeatureStatus = 1 << 16 // config holds the link status
)

const (
	hdrSize       = 12   // struct virtio_net_hdr with num_buffers (VERSION_1)
	legacyHdrSize = 10   // without num_buffers: legacy, MRG_RXBUF not negotiated
	bufSize       = 2048 // header + a 1514-byte frame, rounded up
	queueSize = 128
	mtu       = 1500
	maxFrame  = 1514
)

// Options tune the driver.
type Options struct {
	// Poll, if nonzero, polls the queues instead of using the interrupt:
	// every Poll while frames move, every IdlePoll after a quiet second.
	Poll, IdlePoll time.Duration
}

// ErrDown is returned once the driver instance that published the NIC has
// stopped (killed, or restarting after a failure).
var ErrDown = netdev.ErrDown

var _ netdev.NIC = (*NIC)(nil)

// Driver returns the virtio-net driver for "virtio,device1" nodes.
func Driver(regs func(base uint64) mmio.Regs, opt Options) dev.Driver {
	if opt.Poll > 0 && opt.IdlePoll < opt.Poll {
		opt.IdlePoll = 20 * opt.Poll
	}
	return dev.Driver{Compatible: vmmio.Compatible(DeviceID), Run: func(ctx context.Context, d *dev.Device) error {
		if len(d.Reg) == 0 {
			return errors.New("virtio-net: no registers")
		}
		t, err := vmmio.Init(ctx, regs(d.Reg[0].Base), FeatureMAC|FeatureStatus)
		if err != nil {
			return err
		}
		return run(ctx, d, t, opt)
	}}
}

// PCICompatible is a PCI Ethernet controller (class 02, subclass 00).
const PCICompatible = "pciclass,020000"

// PCIDriver drives a legacy virtio-net PCI function (1af4:1000) with its
// registers in memory BAR0, as Parallels Desktop has. It has no interrupt
// (PCI INTx isn't routed): opt.Poll must be set. regs must give single
// 8- and 16-bit accesses (mmio.NarrowRegs).
func PCIDriver(regs func(base uint64) mmio.Regs, opt Options) dev.Driver {
	if opt.Poll > 0 && opt.IdlePoll < opt.Poll {
		opt.IdlePoll = 20 * opt.Poll
	}
	return dev.Driver{Compatible: PCICompatible, Run: func(ctx context.Context, d *dev.Device) error {
		f, ok := d.Config.(*pci.Function)
		if !ok {
			return errors.New("virtio-net: no PCI function")
		}
		if f.Vendor != vpci.VendorID || f.Device != 0x1000 {
			return fmt.Errorf("virtio-net: %04x:%04x isn't a legacy virtio-net", f.Vendor, f.Device)
		}
		if f.BARs[0].Size == 0 { // drivers/pci assigns memory BARs only
			return errors.New("virtio-net: BAR0 isn't a memory BAR (an I/O BAR has no window on arm64)")
		}
		if opt.Poll == 0 {
			return errors.New("virtio-net: PCI needs Options.Poll (no interrupt routing)")
		}
		r, ok := regs(f.BARs[0].CPU).(mmio.NarrowRegs)
		if !ok {
			return errors.New("virtio-net: registers without 8/16-bit access")
		}
		t, err := vpci.InitLegacy(ctx, r, FeatureMAC|FeatureStatus)
		if err != nil {
			return err
		}
		return run(ctx, d, t, opt)
	}}
}

// transport is what the driver needs of a virtio transport: vmmio.Transport
// (modern virtio-mmio) or vpci.Legacy.
type transport interface {
	Queue(index, max int, a *dma.Allocator) (*vmmio.Queue, error)
	DriverOK()
	Reset()
	Has(f uint64) bool
	Ack() uint32
	Config8(off uint64) uint8
}

// NIC is the published service.
type NIC struct {
	mac  [6]byte
	mode string
	rx   chan []byte
	tx   chan *txReq
	down chan struct{}

	rxFrames, rxDrops, rxBytes, txFrames, txBytes, txFull atomic.Uint64
}

type txReq struct {
	frame []byte
	done  chan error
}

func (n *NIC) MAC() [6]byte { return n.mac }
func (n *NIC) MTU() int     { return mtu }

// Send transmits one Ethernet frame (no FCS). The frame is copied before
// Send returns.
func (n *NIC) Send(ctx context.Context, frame []byte) error {
	if len(frame) > maxFrame {
		return fmt.Errorf("virtio-net: frame of %d bytes", len(frame))
	}
	r := &txReq{frame: frame, done: make(chan error, 1)}
	select {
	case n.tx <- r:
	case <-n.down:
		return ErrDown
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-r.done // accepted: the driver always answers
}

// Recv returns the next received frame.
func (n *NIC) Recv(ctx context.Context) ([]byte, error) {
	select {
	case f := <-n.rx:
		return f, nil
	case <-n.down:
		return nil, ErrDown
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Devices is the lsdev view.
func (n *NIC) Devices() []string {
	return []string{fmt.Sprintf("%s, %s: rx %d frames %d B (%d dropped), tx %d frames %d B (%d ring full)",
		stdnet.HardwareAddr(n.mac[:]), n.mode,
		n.rxFrames.Load(), n.rxBytes.Load(), n.rxDrops.Load(),
		n.txFrames.Load(), n.txBytes.Load(), n.txFull.Load())}
}

// Down is closed when this driver instance stops.
func (n *NIC) Down() <-chan struct{} { return n.down }

type driver struct {
	t        transport
	hdr      int // virtio_net_hdr size
	rxq, txq *vmmio.Queue
	rxBuf    *dma.Buffer // rxq.Size slots of bufSize
	txBuf    *dma.Buffer
	nic      *NIC
}

func run(ctx context.Context, d *dev.Device, t transport, opt Options) error {
	var err error
	defer t.Reset()
	drv := &driver{t: t, hdr: hdrSize}
	if !t.Has(vmmio.FeatureVersion1) {
		drv.hdr = legacyHdrSize
	}
	defer drv.free()
	if drv.rxq, err = t.Queue(0, queueSize, d.DMA); err != nil {
		return err
	}
	if drv.txq, err = t.Queue(1, queueSize, d.DMA); err != nil {
		return err
	}
	if drv.rxBuf, err = d.DMA.Alloc(drv.rxq.Size*bufSize, dma.DefaultAlign); err != nil {
		return err
	}
	if drv.txBuf, err = d.DMA.Alloc(drv.txq.Size*bufSize, dma.DefaultAlign); err != nil {
		return err
	}

	nic := &NIC{rx: make(chan []byte, 64), tx: make(chan *txReq), down: make(chan struct{})}
	defer close(nic.down)
	drv.nic = nic
	if t.Has(FeatureMAC) {
		for i := range nic.mac {
			nic.mac[i] = t.Config8(uint64(i))
		}
	} else {
		rand.Read(nic.mac[:])
		nic.mac[0] = nic.mac[0]&^1 | 2 // unicast, locally administered
	}

	for drv.rxq.Avail() > 0 {
		drv.addRx()
	}

	var irqC <-chan struct{}
	var done func()
	if opt.Poll > 0 {
		nic.mode = fmt.Sprintf("polled every %v (%v idle)", opt.Poll, opt.IdlePoll)
		drv.rxq.SuppressInterrupts(true)
		drv.txq.SuppressInterrupts(true)
	} else {
		id := d.IRQ0()
		if id < 0 || d.Bridge == nil {
			return errors.New("virtio-net: no interrupt (set Options.Poll)")
		}
		line, err := d.Bridge.Request(id, d.Name)
		if err != nil {
			return err
		}
		defer line.Release()
		irqC, done = line.C, line.Done
		nic.mode = fmt.Sprintf("irq %d", id)
	}
	t.DriverOK()
	drv.rxq.Kick()
	d.Ready(nic)

	var tick *time.Ticker
	var tickC <-chan time.Time
	interval := opt.IdlePoll
	if opt.Poll > 0 {
		tick = time.NewTicker(interval)
		defer tick.Stop()
		tickC = tick.C
	}
	lastActive := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-nic.tx:
			drv.reapTx()
			r.done <- drv.send(r.frame)
			lastActive = time.Now()
		case <-irqC:
			t.Ack()
			drv.reapRx()
			drv.reapTx()
			done()
		case now := <-tickC:
			t.Ack()
			if drv.reapRx() > 0 {
				lastActive = now
			}
			drv.reapTx()
		}
		if tick != nil {
			want := opt.IdlePoll
			if time.Since(lastActive) < time.Second {
				want = opt.Poll
			}
			if want != interval {
				interval = want
				tick.Reset(interval)
			}
		}
	}
}

func (drv *driver) free() {
	for _, q := range []*vmmio.Queue{drv.rxq, drv.txq} {
		if q != nil {
			q.Free()
		}
	}
	for _, b := range []*dma.Buffer{drv.rxBuf, drv.txBuf} {
		if b != nil {
			b.Free()
		}
	}
}

// addRx gives one receive buffer to the device. The slot is the descriptor
// index, so the buffer offset is known when it comes back.
func (drv *driver) addRx() {
	if id, ok := drv.rxq.Take(); ok {
		drv.rxq.Push(id, drv.rxBuf.Phys+uint64(id)*bufSize, bufSize, true)
	}
}

// reapRx passes received frames up and recycles their buffers.
func (drv *driver) reapRx() int {
	n := 0
	for {
		id, size, ok := drv.rxq.Used()
		if !ok {
			break
		}
		n++
		if size > drv.hdr && size <= bufSize {
			b := drv.rxBuf.Bytes[int(id)*bufSize:][drv.hdr:size]
			f := append([]byte(nil), b...)
			select {
			case drv.nic.rx <- f:
				drv.nic.rxFrames.Add(1)
				drv.nic.rxBytes.Add(uint64(len(f)))
			default:
				drv.nic.rxDrops.Add(1)
			}
		}
		drv.addRx()
	}
	if n > 0 {
		drv.rxq.Kick()
	}
	return n
}

func (drv *driver) reapTx() {
	for {
		if _, _, ok := drv.txq.Used(); !ok {
			return
		}
	}
}

func (drv *driver) send(frame []byte) error {
	id, ok := drv.txq.Take()
	if !ok {
		drv.nic.txFull.Add(1)
		return errors.New("virtio-net: transmit ring full")
	}
	b := drv.txBuf.Bytes[int(id)*bufSize:][:drv.hdr+len(frame)]
	clear(b[:drv.hdr])
	copy(b[drv.hdr:], frame)
	drv.txq.Push(id, drv.txBuf.Phys+uint64(id)*bufSize, len(b), false)
	drv.txq.Kick()
	drv.nic.txFrames.Add(1)
	drv.nic.txBytes.Add(uint64(len(frame)))
	return nil
}
