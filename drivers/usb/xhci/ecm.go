package xhci

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"godeep/drivers/usb"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/netdev"
)

// USB Ethernet adapters (class drivers only). Two framings carry Ethernet
// frames in bulk transfers:
//
//   - CDC ECM (QEMU's usb-net; Realtek RTL8153 adapters in their second
//     configuration): each transfer is one frame, a zero-length packet ends
//     one that fills whole packets.
//   - CDC NCM (ncm.go: most newer adapters and USB-C docks): transfers are
//     NTBs that bundle frames behind a header.
//
// The communications interface (ECM, NCM) names the MAC address; the data
// interface's alternate setting 1 has the bulk endpoints. Each adapter is
// published as "usbnet<N>" (compatible NICCompatible), a netdev.NIC that
// netd runs the TCP/IP stack on.

// NICCompatible is the device-manager entry for a USB Ethernet adapter.
const NICCompatible = "godeep,usb-ecm"

const (
	ecmFrame        = 1514
	ecmBuf          = 2048
	reqSetInterface = 0x0b
	reqSetFilter    = 0x43 // SET_ETHERNET_PACKET_FILTER
)

// framing turns frames into bulk transfers and back.
type framing interface {
	name() string
	// wrap encodes frame into dst (at least txSize bytes) and returns the
	// transfer's length.
	wrap(dst, frame []byte) int
	// unwrap hands every frame in one received transfer to emit.
	unwrap(b []byte, emit func([]byte))
	txSize() int
	rxSize() int
}

// ecmFraming: one frame per transfer.
type ecmFraming struct{}

func (ecmFraming) name() string                    { return "CDC ECM" }
func (ecmFraming) wrap(dst, frame []byte) int      { return copy(dst, frame) }
func (ecmFraming) unwrap(b []byte, f func([]byte)) { f(b) }
func (ecmFraming) txSize() int                     { return ecmBuf }
func (ecmFraming) rxSize() int                     { return ecmBuf }

// NIC is a USB Ethernet adapter.
type NIC struct {
	h     *hc
	d     *device
	fr    framing
	mac   [6]byte
	out   *bulkEP
	txBuf *dma.Buffer
	rx    chan []byte
	down  chan struct{}
	info  string
	link  atomic.Pointer[string] // link state, when the adapter reports it

	rxFrames, rxDrops, txFrames, rxBad atomic.Uint64
}

type sendReq struct {
	nic   *NIC
	frame []byte
	done  chan error
}

var _ netdev.NIC = (*NIC)(nil)

func (n *NIC) MAC() [6]byte { return n.mac }
func (n *NIC) MTU() int     { return 1500 }

// Send transmits a frame (copied before Send returns).
func (n *NIC) Send(ctx context.Context, frame []byte) error {
	if len(frame) > ecmFrame {
		return fmt.Errorf("usbnet: frame of %d bytes", len(frame))
	}
	r := &sendReq{nic: n, frame: frame, done: make(chan error, 1)}
	select {
	case n.h.sends <- r:
	case <-n.down:
		return netdev.ErrDown
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-r.done
}

func (n *NIC) Recv(ctx context.Context) ([]byte, error) {
	select {
	case f := <-n.rx:
		return f, nil
	case <-n.down:
		return nil, netdev.ErrDown
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (n *NIC) Devices() []string {
	s := fmt.Sprintf("%s, %s, %s: rx %d frames (%d dropped", net.HardwareAddr(n.mac[:]), n.fr.name(), n.info,
		n.rxFrames.Load(), n.rxDrops.Load())
	if b := n.rxBad.Load(); b > 0 {
		s += fmt.Sprintf(", %d bad", b)
	}
	s += fmt.Sprintf("), tx %d frames", n.txFrames.Load())
	if l := n.link.Load(); l != nil {
		s += ", link " + *l
	}
	return []string{s}
}

// deliver queues a received frame for Recv (a copy: b is the DMA buffer).
func (n *NIC) deliver(b []byte) {
	if len(b) < 14 || len(b) > ecmFrame+4 {
		n.rxBad.Add(1)
		return
	}
	select {
	case n.rx <- append([]byte(nil), b...):
		n.rxFrames.Add(1)
	default:
		n.rxDrops.Add(1)
	}
}

func newNIC(h *hc, d *device, fr framing) *NIC {
	return &NIC{h: h, d: d, fr: fr, rx: make(chan []byte, 128), down: make(chan struct{}), info: d.where}
}

// readMAC reads the MAC address string a CDC Ethernet or NCM interface names.
func (h *hc) readMAC(ctx context.Context, d *device, comm usb.Interface) ([6]byte, error) {
	idx, ok := comm.ECMMACIndex()
	if !ok {
		return [6]byte{}, errors.New("no Ethernet functional descriptor")
	}
	buf, err := h.alloc(64, 64)
	if err != nil {
		return [6]byte{}, err
	}
	sd, err := h.control(ctx, d, usb.Setup{RequestType: usb.ReqTypeIn, Request: usb.ReqGetDescriptor,
		Value: uint16(usb.DescString)<<8 | uint16(idx), Index: 0x0409, Length: 64}, buf)
	if err != nil {
		return [6]byte{}, fmt.Errorf("MAC address string: %w", err)
	}
	return usb.ParseMAC(sd)
}

// cdcData finds the data interface of a CDC communications interface (its
// union's subordinate, else the next interface number), alternate setting 1.
func cdcData(cfg usb.Config, comm usb.Interface) (*usb.Interface, error) {
	num := comm.Number + 1
	if s, ok := comm.UnionSubordinate(); ok {
		num = s
	}
	for i := range cfg.Interfaces {
		it := &cfg.Interfaces[i]
		if it.Class == usb.ClassCDCData && it.Number == num && it.Alt == 1 {
			return it, nil
		}
	}
	return nil, errors.New("no data interface")
}

func (h *hc) newECM(ctx context.Context, d *device, cfg usb.Config, comm usb.Interface) error {
	n := newNIC(h, d, ecmFraming{})
	var err error
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
	// directed, broadcast and all-multicast frames (ARP, DHCP need them)
	h.control(ctx, d, usb.Setup{RequestType: 0x21, Request: reqSetFilter, Value: 0x0e, Index: uint16(comm.Number)}, nil)
	h.addNIC(n)
	return nil
}

// startNIC selects the data interface's alternate setting (when it has
// one) and starts reading its bulk IN endpoint.
func (h *hc) startNIC(ctx context.Context, n *NIC, data usb.Interface) error {
	d := n.d
	if data.Alt != 0 {
		if _, err := h.control(ctx, d, usb.Setup{RequestType: 0x01, Request: reqSetInterface, Value: uint16(data.Alt), Index: uint16(data.Number)}, nil); err != nil {
			return fmt.Errorf("select data interface: %w", err)
		}
	}
	var in *endpoint
	for _, e := range data.Endpoints {
		switch {
		case e.IsBulkIn() && in == nil:
			dci, r, err := h.configureEP(ctx, d, e, epBulkIn, [3]uint32{})
			if err != nil {
				return err
			}
			rxBuf, err := h.alloc(n.fr.rxSize(), 64)
			if err != nil {
				return err
			}
			in = &endpoint{slot: d.slot, dci: dci, ring: r, buf: rxBuf, size: n.fr.rxSize()}
			in.handle = func(b []byte, _ time.Time) { n.fr.unwrap(b, n.deliver) }
		case e.IsBulkOut() && n.out == nil:
			dci, r, err := h.configureEP(ctx, d, e, epBulkOut, [3]uint32{})
			if err != nil {
				return err
			}
			n.out = &bulkEP{dci: dci, ring: r, mps: e.MaxPacket}
		}
	}
	if in == nil || n.out == nil {
		return errors.New("no bulk IN and OUT endpoints")
	}
	var err error
	if n.txBuf, err = h.alloc(n.fr.txSize(), 64); err != nil {
		return err
	}
	h.eps = append(h.eps, in)
	h.queueIn(in)
	return nil
}

// send transmits one frame (on the controller's goroutine).
func (n *NIC) send(ctx context.Context, frame []byte) error {
	h := n.h
	l := n.fr.wrap(n.txBuf.Bytes, frame)
	ioc := uint32(trbIOC)
	zlp := l%n.out.mps == 0
	if zlp {
		ioc = 0
	}
	last := n.out.ring.push(n.txBuf.Bus, uint32(l), trbNormal<<10|ioc)
	if zlp { // the zero-length packet ends the transfer
		last = n.out.ring.push(0, 0, trbNormal<<10|trbIOC)
	}
	h.r.Write32(h.db+4*uint64(n.d.slot), uint32(n.out.dci))
	e, err := h.wait(ctx, "ethernet send", func(e event) bool {
		return e.typ() == trbTransferEv && e.slot() == n.d.slot && e.ep() == n.out.dci && e.param == last
	})
	if err != nil {
		return err
	}
	if c := e.code(); c != ccSuccess && c != ccShort {
		return fmt.Errorf("usbnet: send failed with code %d", c)
	}
	n.txFrames.Add(1)
	return nil
}

// NICDriver publishes a USB Ethernet adapter the controller found.
func NICDriver() dev.Driver {
	return dev.Driver{Compatible: NICCompatible, Run: func(ctx context.Context, d *dev.Device) error {
		n, ok := d.Config.(*NIC)
		if !ok {
			return errors.New("usbnet: no adapter")
		}
		d.Ready(n)
		<-ctx.Done()
		return nil
	}}
}

// addNIC publishes a USB Ethernet adapter (any framing) as usbnet<N>.
func (h *hc) addNIC(n *NIC) {
	idx := len(h.nics)
	h.nics = append(h.nics, n)
	h.names.publish(h.publish, "usbnet", idx, dev.Node{Compatible: NICCompatible, Config: n})
}
