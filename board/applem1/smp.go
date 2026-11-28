//go:build tamago && arm64 && applem1

package applem1

import (
	"fmt"
	"runtime"
	"runtime/goos"
	"sync/atomic"
	"time"
	"unsafe"

	"godeep/kernel/fdt"
)

// Every core of the Mac (M1 Pro: 2 efficiency + 8 performance).
//
// m1n1 starts all cores itself (running its per-core init: chicken bits,
// caches) and parks the others on a spin table, as for Linux: each /cpus
// node says enable-method = "spin-table" with a cpu-release-addr. Writing
// an entry point there and sending SEV releases the core, which jumps to
// it at EL2 with its MMU off. secondaryEntry (smp_arm64.s) drops it to EL1
// like the boot core, loads the boot core's MMU, vectors and event-stream
// setting, marks it alive and waits (WFE) for goos.Task to hand it an M.
// No PSCI, no GIC: there is nothing else to set up.
//
// Idle is WFE (idle.go); goos.Wake raises the target's wake flag and sends
// SEV, which ends every core's WFE. The event stream also ends it about
// every 0.7 ms, so a core finds work stolen from another within that even
// without a wake (on TamaGo an idle M keeps its P and polls).
//
// Interrupts are AIC2 polled from a goroutine (drivers/apple/aic), which
// runs on any core: nothing is routed to a particular one.

const maxCPUs = 16

// read by secondaryEntry before its MMU is on (cleaned to memory first)
var (
	smpBoot  [6]uint64 // TTBR0, TCR, MAIR, SCTLR, VBAR, CNTKCTL
	smpMPIDR [maxCPUs]uint64
)

var (
	smpTask  [maxCPUs][4]uint64 // per core: pc (mstart, written last), sp, g, unused
	smpAlive [maxCPUs]uint32
	smpWoken [maxCPUs]uint32 // goos.Wake's flag, cleared by the core's idle
	smpN     uint32
	smpNext  uint32
)

// smpInfo describes the cores in use (lsdev idle, the boot banner). It
// changes at run time when `enable smp` starts the other cores.
var smpInfo atomic.Pointer[string]

func setSMPInfo(s string) { smpInfo.Store(&s) }

// defined in smp_arm64.s
func secondaryEntryAddr() uint64
func save_boot_regs(p *[6]uint64)
func dcache_clean(addr, size uint64)
func dcache_civac(addr uint64)
func sev()
func read_mpidr() uint64

//go:nosplit
func affinity(mpidr uint64) uint64 { return mpidr&0xffffff | (mpidr>>32&0xff)<<32 }

// coreIndex is this core's slot (0: the boot core). Linear search: at
// most 16 entries, and it must not allocate (idle calls it).
//
//go:nosplit
func coreIndex() int {
	a := affinity(read_mpidr())
	n := int(atomic.LoadUint32(&smpN))
	for i := 0; i < n && i < maxCPUs; i++ {
		if smpMPIDR[i] == a {
			return i
		}
	}
	return 0
}

type spinCPU struct {
	aff     uint64
	release uint64
}

func spinTable(root *fdt.Node) (cpus []spinCPU, others int) {
	c := root.Find("/cpus")
	if c == nil {
		return nil, 0
	}
	c.Walk(func(n *fdt.Node) {
		if n.Parent != c || len(n.Strings("device_type")) == 0 || n.Strings("device_type")[0] != "cpu" {
			return
		}
		var reg uint64
		for _, x := range n.U32s("reg") {
			reg = reg<<32 | uint64(x)
		}
		m := n.Strings("enable-method")
		rel := n.U32s("cpu-release-addr")
		if len(m) == 0 || m[0] != "spin-table" || len(rel) != 2 {
			others++
			return
		}
		cpus = append(cpus, spinCPU{aff: affinity(reg), release: uint64(rel[0])<<32 | uint64(rel[1])})
	})
	return cpus, others
}

// smpDefault decides at boot: the Mac stays on its boot core unless
// /chosen/bootargs has godeep.smp (releasing the other cores has hung a
// boot on the hardware: never debugged); the shell's `enable smp` releases
// them later. godeep.nosmp, the old opt-out, is still accepted.
func smpDefault(root *fdt.Node) {
	setSMPInfo("1 core")
	if root == nil {
		return
	}
	cpus, others := spinTable(root)
	if len(cpus)+others < 2 {
		return
	}
	if bootArg(root, "godeep.smp") && !bootArg(root, "godeep.nosmp") {
		startSMP(root)
		return
	}
	setSMPInfo(fmt.Sprintf("1 of %d cores (smp off: `enable smp`, or godeep.smp in bootargs)", len(cpus)+others))
}

// smpStarted: startSMP ran (once only: the cores it released are ours).
var smpStarted atomic.Bool

// startSMP releases every parked core and sets GOMAXPROCS to match. It
// runs once, at boot (godeep.smp) or from `enable smp`; later calls do
// nothing.
func startSMP(root *fdt.Node) {
	if root == nil || !smpStarted.CompareAndSwap(false, true) {
		return
	}
	cpus, others := spinTable(root)
	if len(cpus)+others < 2 {
		return
	}
	self := affinity(read_mpidr())
	save_boot_regs(&smpBoot)
	smpMPIDR[0] = self
	n := uint32(1)
	var failed []string
	for _, c := range cpus {
		if c.aff == self {
			continue
		}
		if n == maxCPUs {
			failed = append(failed, fmt.Sprintf("%#x: over %d cores", c.aff, maxCPUs))
			continue
		}
		if c.release == 0 {
			failed = append(failed, fmt.Sprintf("%#x: no release address", c.aff))
			continue
		}
		blog("smp: core ")
		blogHex(c.aff)
		blog(" release at ")
		blogHex(c.release)
		smpMPIDR[n] = c.aff
		// the core looks itself up in smpMPIDR and reads smpBoot with
		// its MMU off: both must be in memory, not just in our cache
		dcache_clean(uint64(uintptr(unsafe.Pointer(&smpBoot))), uint64(unsafe.Sizeof(smpBoot)))
		dcache_clean(uint64(uintptr(unsafe.Pointer(&smpMPIDR))), uint64(unsafe.Sizeof(smpMPIDR)))
		// release it: the entry point at its cpu-release-addr (in m1n1's
		// memory, mapped non-cacheable), out of every cache, then SEV
		atomic.StoreUint64((*uint64)(unsafe.Pointer(uintptr(c.release))), secondaryEntryAddr())
		dcache_civac(c.release)
		sev()
		alive := false
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			if atomic.LoadUint32(&smpAlive[n]) != 0 {
				alive = true
				break
			}
			runtime.Gosched() // from the shell: let the others run meanwhile
		}
		if alive {
			blog(": up\n")
		} else {
			blog(": didn't start\n")
		}
		if !alive {
			// it may still come up later and wait on its slot: leave the
			// slot (and its MPIDR) to it, and hand it nothing
			failed = append(failed, fmt.Sprintf("%#x: didn't start", c.aff))
			n++
			smpDead |= 1 << (n - 1)
			continue
		}
		n++
	}
	alive := uint32(0)
	for i := uint32(0); i < n; i++ {
		if smpDead&(1<<i) == 0 {
			alive++
		}
	}
	if others > 0 {
		failed = append(failed, fmt.Sprintf("%d cores without a spin table", others))
	}
	if alive < 2 {
		setSMPInfo(fmt.Sprintf("1 of %d cores (%v)", len(cpus)+others, failed))
		return
	}
	atomic.StoreUint32(&smpN, n)
	atomic.StoreUint32(&smpNext, 1)
	goos.ProcID = procID
	goos.Task = task
	goos.Wake = wake
	blog("smp: GOMAXPROCS ")
	blogDec(uint64(alive))
	blog("\n")
	runtime.GOMAXPROCS(int(alive))
	info := fmt.Sprintf("%d of %d cores", alive, len(cpus)+others)
	if len(failed) > 0 {
		info += fmt.Sprintf(" (%v)", failed)
	}
	setSMPInfo(info)
}

// smpDead marks slots whose core didn't start: Task skips them.
var smpDead uint32

//go:nosplit
func procID() uint64 { return uint64(coreIndex()) }

// task hands an M to the next started core (goos.Task).
//
//go:nosplit
func task(sp, _, gp, fn unsafe.Pointer) {
	for {
		i := atomic.AddUint32(&smpNext, 1) - 1
		if i >= atomic.LoadUint32(&smpN) {
			for { // more Ms than cores: can't happen with GOMAXPROCS = cores
			}
		}
		if smpDead&(1<<i) != 0 {
			continue
		}
		t := &smpTask[i]
		t[1] = uint64(uintptr(sp))
		t[2] = uint64(uintptr(gp))
		atomic.StoreUint64(&t[0], uint64(uintptr(fn)))
		sev()
		return
	}
}

// wake ends WFE on core procid (goos.Wake): its flag, then an event.
//
//go:nosplit
func wake(procid uint64) {
	if procid < maxCPUs {
		atomic.StoreUint32(&smpWoken[procid], 1)
	}
	sev()
}

// SMPInfo describes the cores in use.
func SMPInfo() string {
	if p := smpInfo.Load(); p != nil {
		return *p
	}
	return "1 core"
}
