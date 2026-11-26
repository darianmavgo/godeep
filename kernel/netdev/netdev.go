// Package netdev is the interface between network drivers and the TCP/IP
// stack: a NIC moves whole Ethernet frames. Drivers implement it without
// depending on the stack (net/netstack), and the stack runs on any of them.
package netdev

import (
	"context"
	"errors"
)

// NIC is an Ethernet device: whole frames (destination MAC first, no FCS).
type NIC interface {
	MAC() [6]byte
	// MTU is the largest IP packet the device carries (1500 for Ethernet).
	MTU() int
	// Send transmits one frame. It may keep the slice only until it returns.
	Send(ctx context.Context, frame []byte) error
	// Recv returns the next received frame; the caller owns it.
	Recv(ctx context.Context) ([]byte, error)
}

// ErrDown is returned by Send and Recv once the driver instance behind a NIC
// has stopped (killed, or restarting after a failure). The device manager
// publishes a new NIC when the driver comes back.
var ErrDown = errors.New("network device stopped")
