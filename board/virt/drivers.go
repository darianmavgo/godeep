//go:build tamago && arm64 && !applem1

package virt

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"godeep/drivers/arm/pl031"
	"godeep/drivers/arm/pl061"
	"godeep/drivers/nvme"
	"godeep/drivers/pci"
	"godeep/drivers/sdhci"
	"godeep/drivers/simplefb"
	"godeep/drivers/usb/xhci"
	virtiogpu "godeep/drivers/virtio/gpu"
	virtiommio "godeep/drivers/virtio/mmio"
	virtionet "godeep/drivers/virtio/net"
	virtiop9 "godeep/drivers/virtio/p9"
	virtiorng "godeep/drivers/virtio/rng"
	"godeep/kernel/block"
	"godeep/kernel/dev"
	kmmio "godeep/kernel/mmio"
)

func r32(addr uint64) uint32 { return atomic.LoadUint32((*uint32)(unsafe.Pointer(uintptr(addr)))) }
func w32(addr uint64, v uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(uintptr(addr))), v)
}

func init() {
	window := func(base uint64) kmmio.Regs { return kmmio.Window{Base: base} }
	dev.Register(pl061.Driver(window, 3, func() { shutdownOnce.Do(func() { close(shutdown) }) }))
	dev.Register(pl031.Driver(window))
	dev.Register(virtiommio.Driver(window))
	dev.Register(simplefb.Driver())
	dev.Register(virtiorng.Driver(window, virtiorng.Options{Seed: mixSeed}))
	dev.Register(virtiop9.Driver(window))
	dev.Register(virtiogpu.Driver(window))
	dev.Register(dev.Driver{Compatible: "godeep,cpus", Run: func(ctx context.Context, d *dev.Device) error {
		d.Ready(cpuStats{})
		<-ctx.Done()
		return nil
	}})
	// Under HVF, device interrupts trigger QEMU's WFI bug (console.go), so
	// the NIC is polled there: every 2 ms while frames flow, 20 ms when quiet.
	netOpt := virtionet.Options{}
	if OnAppleHVF() {
		netOpt = virtionet.Options{Poll: 2 * time.Millisecond, IdlePoll: 20 * time.Millisecond}
	}
	dev.Register(virtionet.Driver(window, netOpt))
	// Legacy virtio-net on PCI (Parallels Desktop): INTx isn't routed, so
	// it's always polled.
	dev.Register(virtionet.PCIDriver(window, virtionet.Options{Poll: 2 * time.Millisecond, IdlePoll: 20 * time.Millisecond}))
	dev.Register(pci.Driver(window))
	// A disk with serial "godeep-mac" rehearses the Mac SSD's write fence:
	// writes only to the GODEEPDATA FAT32 partition (make check-pcie).
	dev.Register(nvme.Driver(window, func(serial string) block.Policy {
		if serial == "godeep-mac" {
			return block.BasicDataFAT32("GODEEPDATA")
		}
		return nil
	}))
	dev.Register(sdhci.Driver(window, sdhci.Compatible))
	dev.Register(sdhci.Driver(window, sdhci.CompatibleDMA))
	dev.Register(xhci.Driver(window, consoleIn))
	dev.Register(xhci.DiskDriver())
	dev.Register(xhci.NICDriver())
	dev.Register(dev.Driver{Compatible: "arm,armv8-timer", Run: runPtimerProbe})
	dev.Register(dev.Driver{Compatible: "arm,pl011", Run: func(ctx context.Context, d *dev.Device) error {
		d.Ready(dev.Lister(lister{"board console (" + UARTMode + " mode)"}))
		<-ctx.Done()
		return nil
	}})
}

var shutdownOnce sync.Once

type lister []string

func (l lister) Devices() []string { return l }

// ---- ARM physical timer as an interrupt latency probe ----

// PTIMER_IRQ is the EL1 physical timer PPI.
const PTIMER_IRQ = 16 + 14

// defined in timer_arm64.s
func read_cntpct() uint64
func write_cntp_cval(v uint64)
func write_cntp_ctl(v uint64)

type ptimerProbe struct {
	mu   sync.Mutex
	line interface {
		Done()
	}
	c <-chan struct{}
}

// Measure arms the physical timer n times and reports how long after each
// deadline the driver goroutine observed the interrupt: hardware interrupt,
// exception, TamaGo signal relay, the "irq" thread, the bridge, and the
// scheduler, end to end.
func (p *ptimerProbe) Measure(ctx context.Context, n int) ([]time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	freq := read_cntfrq()
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		target := read_cntpct() + freq/1000 // 1ms ahead
		write_cntp_cval(target)
		write_cntp_ctl(1)
		select {
		case <-p.c:
		case <-ctx.Done():
			write_cntp_ctl(0)
			return nil, ctx.Err()
		case <-time.After(time.Second):
			write_cntp_ctl(0)
			return nil, fmt.Errorf("timer interrupt never arrived (sample %d)", i)
		}
		now := read_cntpct()
		write_cntp_ctl(0) // deassert before unmasking the line
		p.line.Done()
		out = append(out, time.Duration((now-target)*1e9/freq))
	}
	return out, nil
}

// runPtimerProbe drives the "arm,armv8-timer" node. Its interrupts are, in
// order: secure physical, non-secure physical, virtual, hypervisor. The
// virtual timer belongs to the idle governor; the probe uses the
// non-secure physical one.
func runPtimerProbe(ctx context.Context, d *dev.Device) error {
	id := PTIMER_IRQ
	if len(d.IRQ) >= 2 {
		id = d.IRQ[1]
	}
	write_cntp_ctl(0)
	line, err := d.Bridge.Request(id, "ptimer")
	if err != nil {
		return err
	}
	defer line.Release()
	d.Ready(dev.LatencyProbe(&ptimerProbe{line: line, c: line.C}))
	<-ctx.Done()
	return nil
}
