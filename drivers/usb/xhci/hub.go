package xhci

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"godeep/drivers/usb"
	"godeep/kernel/dma"
)

// Hubs (USB 2.0 §11): the hub's status-change interrupt endpoint says which
// of its ports changed; the driver asks each for its status, resets newly
// connected ones, and enumerates the device behind them with a route
// string (one 4-bit port number per tier) the controller uses to reach it.
// A low- or full-speed device behind a high-speed hub (any USB-C dock with
// a keyboard or mouse on it) is reached through that hub's transaction
// translator: its slot context names the hub's slot and port (device.tt),
// and the hub's own slot context says how long the TT may think (TTT).

// Hub class requests and features.
const (
	reqGetStatus     = 0x00
	reqClearFeature  = 0x01
	reqSetFeature    = 0x03
	descHub          = 0x29
	featPortReset    = 4
	featPortPower    = 8
	featCPortConnect = 16
	featCPortReset   = 20

	portConnection = 1 << 0
	portEnable     = 1 << 1
	portLowSpeed   = 1 << 9
	portHighSpeed  = 1 << 10

	reqTypeHubClassOther   = 0x23 // host to device, class, other (a port)
	reqTypeHubClassOtherIn = 0xa3
	reqTypeHubClassIn      = 0xa0
)

type hub struct {
	h       *hc
	d       *device
	ports   int
	kids    map[int]*device
	changed bool
	buf     *dma.Buffer
}

func (h *hc) newHub(ctx context.Context, d *device, it usb.Interface) error {
	if d.route>>16 != 0 {
		return errors.New("hubs nest at most five deep")
	}
	buf, err := h.alloc(64, 64)
	if err != nil {
		return err
	}
	b, err := h.control(ctx, d, usb.Setup{RequestType: reqTypeHubClassIn, Request: usb.ReqGetDescriptor, Value: descHub << 8, Length: 9}, buf)
	if err != nil || len(b) < 3 {
		return fmt.Errorf("hub descriptor: %v", err)
	}
	hb := &hub{h: h, d: d, ports: int(b[2]), kids: map[int]*device{}, buf: buf}
	d.hub = hb
	var ttt uint32
	if d.speed == usb.High && len(b) >= 5 { // wHubCharacteristics bits 6:5
		ttt = uint32(binary.LittleEndian.Uint16(b[3:]) >> 5 & 3)
	}
	var status usb.Endpoint
	for _, e := range it.Endpoints {
		if e.IsInterruptIn() {
			status = e
		}
	}
	if status.Address == 0 {
		return errors.New("no status-change endpoint")
	}
	// The slot context says it's a hub with this many ports.
	ep, err := h.interruptIn(ctx, d, status, [3]uint32{slotHub, uint32(hb.ports) << 24, ttt << 16})
	if err != nil {
		return err
	}
	for p := 1; p <= hb.ports; p++ {
		if _, err := h.control(ctx, d, usb.Setup{RequestType: reqTypeHubClassOther, Request: reqSetFeature, Value: featPortPower, Index: uint16(p)}, nil); err != nil {
			return fmt.Errorf("port %d power: %w", p, err)
		}
	}
	sleep(ctx, 100*time.Millisecond) // power-on to power-good
	ep.handle = func([]byte, time.Time) { hb.changed = true }
	h.eps = append(h.eps, ep)
	h.hubs = append(h.hubs, hb)
	h.queueIn(ep)
	hb.scan(ctx) // devices already connected
	return nil
}

func (hb *hub) portStatus(ctx context.Context, p int) (status, change uint16, err error) {
	b, err := hb.h.control(ctx, hb.d, usb.Setup{RequestType: reqTypeHubClassOtherIn, Request: reqGetStatus, Index: uint16(p), Length: 4}, hb.buf)
	if err != nil || len(b) < 4 {
		return 0, 0, fmt.Errorf("port %d status: %v", p, err)
	}
	return binary.LittleEndian.Uint16(b), binary.LittleEndian.Uint16(b[2:]), nil
}

func (hb *hub) feature(ctx context.Context, req byte, feat, p int) error {
	_, err := hb.h.control(ctx, hb.d, usb.Setup{RequestType: reqTypeHubClassOther, Request: req, Value: uint16(feat), Index: uint16(p)}, nil)
	return err
}

// scan looks at every port: enumerates new devices, forgets removed ones.
func (hb *hub) scan(ctx context.Context) {
	h := hb.h
	for p := 1; p <= hb.ports; p++ {
		st, ch, err := hb.portStatus(ctx, p)
		if err != nil {
			h.svc.addLine(fmt.Sprintf("%s: %v", hb.d.where, err))
			return
		}
		if ch&1 != 0 {
			hb.feature(ctx, reqClearFeature, featCPortConnect, p)
		}
		where := fmt.Sprintf("%s.%d", hb.d.where, p)
		if st&portConnection == 0 {
			if kid, ok := hb.kids[p]; ok {
				h.detach(ctx, kid)
				delete(hb.kids, p)
				h.svc.addLine(where + ": disconnected")
			}
			continue
		}
		if _, ok := hb.kids[p]; ok {
			continue
		}
		speed, err := hb.reset(ctx, p)
		if err != nil {
			hb.kids[p] = &device{} // don't retry every change
			h.svc.addLine(fmt.Sprintf("%s: %v", where, err))
			continue
		}
		depth := 0
		for r := hb.d.route; r != 0; r >>= 4 {
			depth++
		}
		route := hb.d.route | uint32(p)<<(4*depth)
		d, err := h.enumerate(ctx, hb.d.port, route, where, speed, hb.d.childTT(p, speed))
		if d == nil {
			d = &device{}
		}
		hb.kids[p] = d
		if err != nil {
			h.svc.addLine(fmt.Sprintf("%s: %v", where, err))
		}
	}
}

// reset resets a hub port and returns the speed of what's on it.
func (hb *hub) reset(ctx context.Context, p int) (usb.Speed, error) {
	if err := hb.feature(ctx, reqSetFeature, featPortReset, p); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(time.Second)
	for {
		if err := sleep(ctx, 10*time.Millisecond); err != nil {
			return 0, err
		}
		st, ch, err := hb.portStatus(ctx, p)
		if err != nil {
			return 0, err
		}
		if ch&(1<<4) != 0 { // C_PORT_RESET
			hb.feature(ctx, reqClearFeature, featCPortReset, p)
			if st&portEnable == 0 {
				return 0, errors.New("port not enabled after reset")
			}
			sleep(ctx, 10*time.Millisecond) // reset recovery
			switch {
			case st&portLowSpeed != 0:
				return usb.Low, nil
			case st&portHighSpeed != 0:
				return usb.High, nil
			}
			return usb.Full, nil
		}
		if time.Now().After(deadline) {
			return 0, errors.New("port reset timed out")
		}
	}
}
