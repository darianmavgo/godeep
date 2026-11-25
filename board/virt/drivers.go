//go:build tamago && arm64

package virt

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"godeep/kernel/dev"
)

func r32(addr uint64) uint32 { return atomic.LoadUint32((*uint32)(unsafe.Pointer(uintptr(addr)))) }
func w32(addr uint64, v uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(uintptr(addr))), v)
}

func init() {
	dev.Register(dev.Driver{Compatible: "arm,pl061", Run: runPL061})
	dev.Register(dev.Driver{Compatible: "arm,pl031", Run: runPL031})
	dev.Register(dev.Driver{Compatible: "virtio,mmio-bus", Run: runVirtioScan})
	dev.Register(dev.Driver{Compatible: "arm,armv8-ptimer-probe", Run: runPtimerProbe})
}

// ---- PL061 GPIO: the power button (line 3), interrupt-driven ----

const (
	gpioIS  = 0x404 // interrupt sense (0 = edge)
	gpioIBE = 0x408 // both edges
	gpioIEV = 0x40c // event (1 = rising)
	gpioIE  = 0x410 // interrupt enable
	gpioMIS = 0x418 // masked interrupt status
	gpioIC  = 0x41c // interrupt clear

	powerButton = 1 << 3
)

var shutdownOnce sync.Once

func runPL061(ctx context.Context, d *dev.Device) error {
	base := d.Reg[0].Base
	line, err := d.Bridge.Request(d.IRQ0(), "gpio0")
	if err != nil {
		return err
	}
	defer line.Release()
	w32(base+gpioIS, r32(base+gpioIS)&^powerButton)
	w32(base+gpioIBE, r32(base+gpioIBE)&^powerButton)
	w32(base+gpioIEV, r32(base+gpioIEV)|powerButton)
	w32(base+gpioIC, powerButton)
	w32(base+gpioIE, r32(base+gpioIE)|powerButton)
	d.Ready("power button")
	for {
		select {
		case <-line.C:
			mis := r32(base + gpioMIS)
			w32(base+gpioIC, mis)
			line.Done()
			if mis&powerButton != 0 {
				shutdownOnce.Do(func() { close(shutdown) })
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// ---- PL031 RTC: wall clock ----

type rtc struct{ base uint64 }

func (r rtc) Now() time.Time { return time.Unix(int64(r32(r.base)), 0) }

func runPL031(ctx context.Context, d *dev.Device) error {
	d.Ready(dev.Clock(rtc{d.Reg[0].Base}))
	<-ctx.Done()
	return nil
}

// ---- virtio-mmio: discover what QEMU attached ----

var virtioNames = map[uint32]string{1: "net", 2: "blk", 3: "console", 4: "rng", 9: "9p", 16: "gpu", 18: "input"}

type virtioBus []string

func (v virtioBus) Devices() []string { return v }

func runVirtioScan(ctx context.Context, d *dev.Device) error {
	var found virtioBus
	for slot := uint64(0); slot < VIRTIO_MMIO_SLOTS; slot++ {
		base := d.Reg[0].Base + slot*0x200
		if r32(base) != 0x74726976 { // "virt"
			continue
		}
		id := r32(base + 0x008)
		if id == 0 {
			continue // empty slot
		}
		name := virtioNames[id]
		if name == "" {
			name = fmt.Sprintf("id%d", id)
		}
		found = append(found, fmt.Sprintf("%s@slot%d(irq %d)", name, slot, VIRTIO_IRQ+int(slot)))
	}
	if len(found) == 0 {
		found = virtioBus{"no devices attached"}
	}
	d.Ready(dev.Lister(found))
	return nil // probe-only: drivers for each device come later
}

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

func runPtimerProbe(ctx context.Context, d *dev.Device) error {
	write_cntp_ctl(0)
	line, err := d.Bridge.Request(d.IRQ0(), "ptimer")
	if err != nil {
		return err
	}
	defer line.Release()
	d.Ready(dev.LatencyProbe(&ptimerProbe{line: line, c: line.C}))
	<-ctx.Done()
	return nil
}
