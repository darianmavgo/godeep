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

// USB Ethernet, CDC ECM (the class QEMU's usb-net offers, and that the
// Mac's DWC3 gadget will speak from the other side): the communications
// interface names the MAC address, the data interface's alternate setting
// 1 has a bulk IN and a bulk OUT endpoint, and each transfer is one
// Ethernet frame (a zero-length packet ends one that fills whole packets).
// The adapter is published as "usbnet<N>" (compatible NICCompatible), a
// netdev.NIC that netd can run the TCP/IP stack on.

// NICCompatible is the device-manager entry for a USB Ethernet adapter.
const NICCompatible = "godeep,usb-ecm"

const (
	ecmFrame        = 1514
	ecmBuf          = 2048
	reqSetInterface = 0x0b
	reqSetFilter    = 0x43 // SET_ETHERNET_PACKET_FILTER
)

// NIC is a USB Ethernet adapter.
type NIC struct {
	h     *hc
	d     *device
	mac   [6]byte
	out   *bulkEP
	txBuf *dma.Buffer
	rx    chan []byte
	down  chan struct{}
	info  string

	rxFrames, rxDrops, txFrames atomic.Uint64
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
		return fmt.Errorf("usb-ecm: frame of %d bytes", len(frame))
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
	return []string{fmt.Sprintf("%s, %s: rx %d frames (%d dropped), tx %d frames",
		net.HardwareAddr(n.mac[:]), n.info, n.rxFrames.Load(), n.rxDrops.Load(), n.txFrames.Load())}
}

func (h *hc) newECM(ctx context.Context, d *device, cfg usb.Config, comm usb.Interface) error {
	n := &NIC{h: h, d: d, rx: make(chan []byte, 64), down: make(chan struct{}), info: d.where}
	idx, ok := comm.ECMMACIndex()
	if !ok {
		return errors.New("no Ethernet functional descriptor")
	}
	buf, err := h.alloc(64, 64)
	if err != nil {
		return err
	}
	sd, err := h.control(ctx, d, usb.Setup{RequestType: usb.ReqTypeIn, Request: usb.ReqGetDescriptor,
		Value: uint16(usb.DescString)<<8 | uint16(idx), Index: 0x0409, Length: 64}, buf)
	if err != nil {
		return fmt.Errorf("MAC address string: %w", err)
	}
	if n.mac, err = usb.ParseMAC(sd); err != nil {
		return err
	}
	// The data interface: the one right after, alternate setting 1.
	var data *usb.Interface
	for i := range cfg.Interfaces {
		it := &cfg.Interfaces[i]
		if it.Class == usb.ClassCDCData && it.Number == comm.Number+1 && it.Alt == 1 {
			data = it
		}
	}
	if data == nil {
		return errors.New("no data interface")
	}
	if _, err := h.control(ctx, d, usb.Setup{RequestType: 0x01, Request: reqSetInterface, Value: 1, Index: uint16(data.Number)}, nil); err != nil {
		return fmt.Errorf("select data interface: %w", err)
	}
	for _, e := range data.Endpoints {
		switch {
		case e.IsBulkIn():
			dci, r, err := h.configureEP(ctx, d, e, epBulkIn, [2]uint32{})
			if err != nil {
				return err
			}
			rxBuf, err := h.alloc(ecmBuf, 64)
			if err != nil {
				return err
			}
			ep := &endpoint{slot: d.slot, dci: dci, ring: r, buf: rxBuf, size: ecmBuf}
			ep.handle = func(frame []byte, _ time.Time) {
				f := append([]byte(nil), frame...)
				select {
				case n.rx <- f:
					n.rxFrames.Add(1)
				default:
					n.rxDrops.Add(1)
				}
			}
			h.eps = append(h.eps, ep)
			h.queueIn(ep)
		case e.IsBulkOut():
			dci, r, err := h.configureEP(ctx, d, e, epBulkOut, [2]uint32{})
			if err != nil {
				return err
			}
			n.out = &bulkEP{dci: dci, ring: r, mps: e.MaxPacket}
		}
	}
	if n.out == nil {
		return errors.New("no bulk OUT endpoint")
	}
	if n.txBuf, err = h.alloc(ecmBuf, 64); err != nil {
		return err
	}
	// directed, broadcast and all-multicast frames (ARP, DHCP need them)
	h.control(ctx, d, usb.Setup{RequestType: 0x21, Request: reqSetFilter, Value: 0x0e, Index: uint16(comm.Number)}, nil)
	name := fmt.Sprintf("usbnet%d", len(h.nics))
	h.nics = append(h.nics, n)
	if h.addDev != nil {
		h.addDev(dev.Node{Name: name, Compatible: NICCompatible, Config: n})
	}
	return nil
}

// send transmits one frame (on the controller's goroutine).
func (n *NIC) send(ctx context.Context, frame []byte) error {
	h := n.h
	copy(n.txBuf.Bytes, frame)
	ioc := uint32(trbIOC)
	zlp := len(frame)%n.out.mps == 0
	if zlp {
		ioc = 0
	}
	last := n.out.ring.push(n.txBuf.Phys, uint32(len(frame)), trbNormal<<10|ioc)
	if zlp { // the zero-length packet ends the frame
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
		return fmt.Errorf("usb-ecm: send failed with code %d", c)
	}
	n.txFrames.Add(1)
	return nil
}

// NICDriver publishes a USB Ethernet adapter the controller found.
func NICDriver() dev.Driver {
	return dev.Driver{Compatible: NICCompatible, Run: func(ctx context.Context, d *dev.Device) error {
		n, ok := d.Config.(*NIC)
		if !ok {
			return errors.New("usb-ecm: no adapter")
		}
		d.Ready(n)
		<-ctx.Done()
		return nil
	}}
}
