package kernel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"godeep/kernel/cap"
	"godeep/kernel/dev"
	"godeep/kernel/netdev"
	"godeep/kernel/proc"
	"godeep/net/netstack"
)

// nicWait is how long netd waits for a network device to appear; dhcpWait
// how long it tries DHCP before falling back to the static setup.
const (
	nicWait  = 10 * time.Second
	dhcpWait = 8 * time.Second
)

// startNet starts "netd": it waits for the first device whose service is a
// netdev.NIC, runs the TCP/IP stack on it, configures it (cfg if it has an
// address; else DHCP, then fallback if DHCP fails), and resolves the
// returned PendingNet with it (or with the error saying why there is none).
func startNet(ctx context.Context, tab *proc.Table, mgr *dev.Manager, cfg, fallback netstack.Config, log io.Writer) *cap.PendingNet {
	pn := cap.NewPendingNet()
	tab.Spawn(ctx, 0, proc.Spec{Name: "netd", Stdout: log, Main: func(ctx context.Context, p *proc.Proc) error {
		name, nic, err := findNIC(ctx, mgr, nicWait)
		if err != nil {
			pn.Resolve(nil, err)
			return nil
		}
		st, err := netstack.New(&followNIC{mgr: mgr, name: name, cur: nic}, cfg)
		if err != nil {
			pn.Resolve(nil, err)
			return err
		}
		defer st.Close()
		p.Go(func(ctx context.Context) {
			if err := st.RunRX(ctx); err != nil {
				fmt.Fprintf(log, "netd: receive stopped: %v\n", err)
			}
		})
		p.Go(func(ctx context.Context) { st.RunTX(ctx) })
		if !cfg.Addr.IsValid() {
			dctx, cancel := context.WithTimeout(ctx, dhcpWait)
			_, err := st.DHCP(dctx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				fmt.Fprintf(log, "netd: DHCP failed (%v); using %s\n", err, fallback.Addr)
				if err := st.Configure(fallback, "static (DHCP failed)"); err != nil {
					pn.Resolve(nil, err)
					return err
				}
			}
		}
		pn.Resolve(st, nil)
		fmt.Fprintf(log, "netd: %s up, %s\n", name, st.Devices()[0])
		<-ctx.Done()
		return nil
	}})
	return pn
}

// findNIC polls the device manager until a ready device publishes a NIC.
func findNIC(ctx context.Context, mgr *dev.Manager, wait time.Duration) (string, netdev.NIC, error) {
	deadline := time.Now().Add(wait)
	for {
		for _, s := range mgr.List() {
			if nic, ok := s.Service.(netdev.NIC); ok && s.State == dev.Ready {
				return s.Name, nic, nil
			}
		}
		if time.Now().After(deadline) {
			return "", nil, fmt.Errorf("%w: no network device", cap.ErrNoNetwork)
		}
		t := time.NewTimer(50 * time.Millisecond)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return "", nil, ctx.Err()
		}
	}
}

// followNIC is the NIC the stack sees: when the driver behind it restarts
// (its NIC returns netdev.ErrDown), it waits for the device to be ready
// again and carries on with the new instance, so open connections survive
// a driver restart.
type followNIC struct {
	mgr  *dev.Manager
	name string

	mu  sync.Mutex
	cur netdev.NIC
}

func (f *followNIC) nic() netdev.NIC { f.mu.Lock(); defer f.mu.Unlock(); return f.cur }
func (f *followNIC) MAC() [6]byte    { return f.nic().MAC() }
func (f *followNIC) MTU() int        { return f.nic().MTU() }

func (f *followNIC) Send(ctx context.Context, frame []byte) error {
	for {
		n := f.nic()
		err := n.Send(ctx, frame)
		if !errors.Is(err, netdev.ErrDown) {
			return err
		}
		if err := f.rebind(ctx, n); err != nil {
			return err
		}
	}
}

func (f *followNIC) Recv(ctx context.Context) ([]byte, error) {
	for {
		n := f.nic()
		frame, err := n.Recv(ctx)
		if !errors.Is(err, netdev.ErrDown) {
			return frame, err
		}
		if err := f.rebind(ctx, n); err != nil {
			return nil, err
		}
	}
}

// rebind waits for a NIC other than old (the restarted driver's).
func (f *followNIC) rebind(ctx context.Context, old netdev.NIC) error {
	for {
		if f.nic() != old {
			return nil // the other pump already rebound
		}
		svc, err := f.mgr.Get(ctx, f.name)
		if err != nil {
			return err
		}
		if n, ok := svc.(netdev.NIC); ok && n != old {
			f.mu.Lock()
			if f.cur == old {
				f.cur = n
			}
			f.mu.Unlock()
			return nil
		}
		t := time.NewTimer(20 * time.Millisecond)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}
