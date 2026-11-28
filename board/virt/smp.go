//go:build tamago && arm64 && !applem1

package virt

import (
	"fmt"
	"runtime"
	"runtime/goos"
	"sync/atomic"
	"time"
	"unsafe"

	"godeep/kernel/fdt"
)

// SMP on QEMU virt (-smp N). The Go runtime on TamaGo already runs one M
// per P and asks the board for each extra M through goos.Task; the board:
//
//   - starts every core in the device tree with PSCI CPU_ON at
//     secondaryEntry (smp_arm64.s), which loads the boot core's MMU and
//     vector registers, enables its GIC CPU interface, marks itself alive
//     and sleeps (WFE) until Task hands it an M: a stack, g0 and mstart;
//   - wakes the core's GIC redistributor and enables, there, the wake-up
//     SGI and the virtual-timer PPI that end its WFI;
//   - sets goos.ProcID (the core's affinity), goos.Task and goos.Wake (an
//     SGI that ends the target's WFI in idle), then GOMAXPROCS = cores.
//
// Secondaries never unmask IRQs: device interrupts stay routed to the boot
// core and acknowledged in its exception (irq.go).

// maxCPUs sizes the static per-core slots (Task and secondaryEntry run where
// nothing can be allocated); it matches the redistributor scan below.
const maxCPUs = 64

// boot core registers for secondaryEntry (read with the MMU off: cleaned
// to memory first)
var smpBoot struct{ ttbr0, tcr, mair, sctlr, vbar uint64 }

var (
	smpTask  [maxCPUs][4]uint64 // per core: pc (mstart, written last), sp, g, unused
	smpAlive [maxCPUs]uint32    // set by each core once its MMU is on
	smpN     uint32             // cores running Go (boot core included)
	smpNext  uint32             // next core Task hands an M to
	smpInfo  = "1 core"
)

// defined in smp_arm64.s
func secondaryEntryAddr() uint64
func save_boot_regs(p *[5]uint64)
func dcache_clean(addr, size uint64)
func sev()

const psciCPUOn = 0xc4000003

// GIC redistributor registers
const (
	gicrStride = 0x20000
	gicrTYPER  = 0x08
	gicrWAKER  = 0x14
)

// cpuMPIDRs lists the cores in the device tree (their MPIDR affinity).
func cpuMPIDRs() []uint64 {
	if bootFDT == 0 {
		return nil
	}
	hdr := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), 8)
	size := uint32(hdr[4])<<24 | uint32(hdr[5])<<16 | uint32(hdr[6])<<8 | uint32(hdr[7])
	root, err := fdt.Parse(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(bootFDT))), size))
	if err != nil {
		return nil
	}
	var out []uint64
	if cpus := root.Find("/cpus"); cpus != nil {
		cpus.Walk(func(n *fdt.Node) {
			if n.Parent != cpus || len(n.Strings("device_type")) == 0 || n.Strings("device_type")[0] != "cpu" {
				return
			}
			var v uint64
			for _, c := range n.U32s("reg") {
				v = v<<32 | uint64(c)
			}
			out = append(out, affinity(v))
		})
	}
	return out
}

// redistributor finds the GIC redistributor frame of the core with
// affinity aff (GICR_TYPER[63:32] holds Aff3.Aff2.Aff1.Aff0).
func redistributor(aff uint64) (uint64, bool) {
	want := (aff>>32&0xff)<<24 | aff&0xffffff
	for i := uint64(0); i < 64; i++ {
		base := GICR_BASE + i*gicrStride
		typer := atomic.LoadUint64((*uint64)(unsafe.Pointer(uintptr(base + gicrTYPER))))
		if typer>>32 == want {
			return base, true
		}
		if typer&(1<<4) != 0 { // Last
			break
		}
	}
	return 0, false
}

// wakeRedistributor marks a core's redistributor awake and enables the
// interrupts that end its WFI: the wake-up SGI and its virtual timer.
func wakeRedistributor(base uint64) error {
	w32(base+gicrWAKER, r32(base+gicrWAKER)&^(1<<1)) // ProcessorSleep = 0
	for deadline := time.Now().Add(time.Second); r32(base+gicrWAKER)&(1<<2) != 0; {
		if time.Now().After(deadline) {
			return fmt.Errorf("redistributor %#x stays asleep", base)
		}
	}
	bits := uint32(1)<<wakeSGI | uint32(1)<<VTIMER_IRQ
	sgi := base + gicrSGIBase
	w32(sgi+gicdIGROUPR, r32(sgi+gicdIGROUPR)&^bits) // Group 0, like the boot core
	w32(sgi+gicdISENABLER, bits)
	return nil
}

// startSMP brings up every other core and raises GOMAXPROCS to match. It
// runs once, on the boot core, before anything else is scheduled.
func startSMP() {
	cpus := cpuMPIDRs()
	if len(cpus) < 2 {
		return
	}
	if bootEL != 1 {
		smpInfo = fmt.Sprintf("1 of %d cores (SMP needs an EL1 boot)", len(cpus))
		return
	}
	self := affinity(read_mpidr())
	var regs [5]uint64
	save_boot_regs(&regs)
	smpBoot.ttbr0, smpBoot.tcr, smpBoot.mair, smpBoot.sctlr, smpBoot.vbar = regs[0], regs[1], regs[2], regs[3], regs[4]
	// secondaries read these (and their own slots) before their MMU is on
	dcache_clean(uint64(uintptr(unsafe.Pointer(&smpBoot))), uint64(unsafe.Sizeof(smpBoot)))
	dcache_clean(uint64(uintptr(unsafe.Pointer(&smpAlive))), uint64(unsafe.Sizeof(smpAlive)))

	n := uint32(1)
	var failed []string
	// The boot core takes wake-up SGIs too (goos.Wake(boot) when work or a
	// stop-the-world waits for it): without SGI 0 enabled in its own
	// redistributor it slept on until its next timer.
	if base, ok := redistributor(self); ok {
		if err := wakeRedistributor(base); err != nil {
			failed = append(failed, err.Error())
		}
	}
	for _, aff := range cpus {
		if aff == self || n == maxCPUs {
			continue
		}
		base, ok := redistributor(aff)
		if !ok {
			failed = append(failed, fmt.Sprintf("%#x: no redistributor", aff))
			continue
		}
		if err := wakeRedistributor(base); err != nil {
			failed = append(failed, err.Error())
			continue
		}
		mpidr := aff&0xffffff | (aff>>32&0xff)<<32
		if r := psci4(psciCPUOn, mpidr, secondaryEntryAddr(), uint64(n)); r != 0 {
			failed = append(failed, fmt.Sprintf("%#x: CPU_ON %d", aff, int64(r)))
			continue
		}
		alive := false
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			if atomic.LoadUint32(&smpAlive[n]) != 0 {
				alive = true
				break
			}
		}
		if !alive {
			failed = append(failed, fmt.Sprintf("%#x: didn't start", aff))
			continue // its slot stays unused: Task never hands it an M
		}
		n++
	}
	if n == 1 {
		smpInfo = fmt.Sprintf("1 of %d cores (%v)", len(cpus), failed)
		return
	}
	atomic.StoreUint32(&smpN, n)
	atomic.StoreUint32(&smpNext, 1)
	goos.ProcID = procID
	goos.Task = task
	goos.Wake = wake
	runtime.GOMAXPROCS(int(n))
	smpInfo = fmt.Sprintf("%d cores", n)
	if len(failed) > 0 {
		smpInfo += fmt.Sprintf(" (%v)", failed)
	}
}

// procID is the core's affinity (goos.ProcID, stored in each M).
//
//go:nosplit
func procID() uint64 { return affinity(read_mpidr()) }

// task hands an M to the next waiting core (goos.Task): it runs fn
// (mstart) on stack sp with g = gp. Ms are bound to Ps and never exit on
// TamaGo, so with GOMAXPROCS = cores this runs once per secondary.
//
//go:nosplit
func task(sp, _, gp, fn unsafe.Pointer) {
	i := atomic.AddUint32(&smpNext, 1) - 1
	if i >= atomic.LoadUint32(&smpN) {
		for { // more Ms than cores: can't happen with GOMAXPROCS = cores
		}
	}
	t := &smpTask[i]
	t[1] = uint64(uintptr(sp))
	t[2] = uint64(uintptr(gp))
	atomic.StoreUint64(&t[0], uint64(uintptr(fn))) // release: sp and g first
	sev()
}

// wake ends WFI on the core with affinity procid (goos.Wake): a Group 0
// SGI. In ICC_SGI0R_EL1: Aff3 55:48, RS 47:44, Aff2 39:32, INTID 27:24,
// Aff1 23:16, target list 15:0 (Aff0 & 15).
//
//go:nosplit
func wake(procid uint64) {
	atomic.AddUint32(&cntWakeSent[coreIndex(procid)], 1)
	aff0 := procid & 0xff
	v := (procid>>32&0xff)<<48 | (aff0>>4)<<44 | (procid>>16&0xff)<<32 |
		uint64(wakeSGI)<<24 | (procid>>8&0xff)<<16 | 1<<(aff0&15)
	icc_sgi0r(v)
}

// Per-core counters (index: Aff0), shown by lsdev cpus.
var (
	cntWakeSent [maxCPUs]uint32 // goos.Wake aimed at the core
	cntSGI      [maxCPUs]uint32 // wake-up SGIs it acknowledged
	cntTimer    [maxCPUs]uint32 // idle timer expiries it acknowledged
	cntIdle     [maxCPUs]uint32 // idle entries (WFI)
	idleTicks   [maxCPUs]uint64 // counter ticks asleep in WFI (dev.CPULoad)
)

//go:nosplit
func coreIndex(aff uint64) uint64 { return aff & 0xff % maxCPUs }

// cpuStats is the "cpus" device's service (lsdev cpus).
type cpuStats struct{}

func (cpuStats) Devices() []string {
	out := []string{smpInfo}
	n := max(1, int(atomic.LoadUint32(&smpN)))
	for i := 0; i < n && i < maxCPUs; i++ {
		out = append(out, fmt.Sprintf("cpu%d: idle %d, wakes sent %d, SGIs %d, timer %d", i,
			atomic.LoadUint32(&cntIdle[i]), atomic.LoadUint32(&cntWakeSent[i]),
			atomic.LoadUint32(&cntSGI[i]), atomic.LoadUint32(&cntTimer[i])))
	}
	return out
}

// CPUIdle is each core's time asleep in WFI since boot (dev.CPULoad).
func (cpuStats) CPUIdle() []time.Duration {
	n := max(1, int(atomic.LoadUint32(&smpN)))
	freq := read_cntfrq()
	out := make([]time.Duration, min(n, maxCPUs))
	for i := range out {
		t := atomic.LoadUint64(&idleTicks[i])
		out[i] = time.Duration(t/freq*1e9 + t%freq*1e9/freq)
	}
	return out
}

// SMPInfo describes the cores in use (for the boot banner).
func SMPInfo() string { return smpInfo }
