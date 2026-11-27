# SMP on Apple Silicon: Asahi Linux, XNU, TamaGo (and godeep)

How each system starts the other cores, sends cross-core wake-ups (IPIs),
idles, and schedules. Written 2026-11-27 from the sources:

- m1n1 `src/smp.c`, `smp.h`, `kboot.c`
- Linux `arch/arm64/kernel/smp_spin_table.c`, `drivers/irqchip/irq-apple-aic.c`,
  `arch/arm64/boot/dts/apple/t600x-common.dtsi`, Asahi `drivers/cpuidle/cpuidle-apple.c`
- XNU (`apple-oss-distributions/xnu`, main) `osfmk/arm64/{cpu.c,start.s,machine_routines.c,locore.s,sleh.c}`,
  `osfmk/arm/cpu_common.c`, `osfmk/kern/{sched_prim.c,sched_amp.c,sched_clutch.c}`, `iokit/Kernel/IOCPU.cpp`
- tamago-go `src/runtime/{os_tamago.go,idle_tamago.go,goos/stub.go}`, tamago v1.27.4 `amd64/smp.go`

Items marked *(unverified)* were not confirmed in the code that was read.

## At a glance

| | Asahi Linux (m1n1 + Linux) | XNU | TamaGo | godeep |
|---|---|---|---|---|
| Who powers the core on | m1n1 (PMGR CPU-start registers) | XNU, via a platform driver behind `PE_cpu_start_internal` | amd64: the BSP (INIT-SIPI). arm64: nobody (no support) | m1n1 on the Mac; PSCI `CPU_ON` on QEMU virt |
| Where a new core starts | RVBAR → m1n1 `_vectors_start`, then a spin table | RVBAR → `LowResetVectorBase` → `start_cpu` | amd64: 16-bit `apinit` at 0x4000, then `apstart` waits for an NMI | `secondaryEntry` (`board/*/smp_arm64.s`) |
| Hand-off to the kernel | spin table: `cpu-release-addr` + SEV | none (XNU owns the core from reset) | `goos.Task` (sp, g, mstart) | spin table (Mac) or PSCI entry (QEMU), then `goos.Task` |
| Cross-core wake-up | "fast IPI" system registers, delivered as FIQ | same fast IPI registers, as FIQ | `goos.Wake` (amd64: LAPIC IPI) | Mac: per-core flag + SEV; QEMU: GIC SGI |
| Timer interrupt | generic timer, delivered as FIQ | generic timer, FIQ (`rtclock_intr`) | not used by the scheduler | not used (Mac: WFE + event stream) |
| Idle | WFI, or deep WFI (core power-down) via cpuidle | WFE until the timer, then WFI / retention | `goos.Idle(until)` | WFE (Mac), WFI + vtimer (QEMU) |
| Scheduler | Linux CFS/EEVDF, preemptive, capacity-aware | Mach: Clutch / Edge or AMP, preemptive (AST) | Go GMP, **no preemption** | Go GMP via TamaGo |

## 1. Asahi Linux

Asahi splits the work: **m1n1** (the boot loader) starts every core, and
**Linux** only releases cores that are already running.

### m1n1 starts the cores (`smp.c`)

- **Finding the cores.** m1n1 walks the Apple device tree (ADT) `/cpus`.
  - Each core's index is `cpu-id` (falling back to `reg`).
  - The ADT `reg` field encodes core (bits 7:0), cluster (10:8) and die (14:11).
  - The boot core is the one whose `state` is `"running"`.
  - Each core's implementation registers come from `cpu-impl-reg` (fallback: `/arm-io` `reg`).
- **Reset vector.** RVBAR sits in each core's impl registers:
  `RVBAR_ADDR` is bits 47:12 and `RVBAR_LOCK` is bit 0.
  - If Apple's system registers are unlocked, m1n1 writes its own `_vectors_start` there.
  - If RVBAR is locked to a different address, m1n1 prints a warning and continues.
- **Power-on.** This goes through the PMGR "CPU start" block at
  `pmgr reg[0] + CPU_START_OFF`. The offset is 0x54000 on t8103/t6000/t6001/t6002 (M1, M1 Pro/Max/Ultra);
  each die is `PMGR_DIE_OFFSET` further on.
  1. `write32(base + 0x4, 1 << (4*cluster + core))`: a system-level startup bit. Without it, "IRQs don't work".
  2. `write32(base + 0x8 + 4*cluster, 1 << core)`: actually starts the core.
  3. m1n1 then polls the core's spin-table `flag` for up to 100 ms.
- **What a started core does.** It runs `smp_secondary_entry` with a private
  stack and announces itself by incrementing `spin_table[i].flag`. The spin table entry is
  `struct spin_table { mpidr, flag, target, args[4], retval }`. The core then loops:
  - **In WFE mode:** it waits with `wfe` until `target != 0`.
  - **Otherwise:** it waits in `deep_wfi()` and acknowledges IPIs.
  - When `target` is set, it calls it as a C function, `target(args[0..3])`, stores the result in `retval`, and clears `target`.
  - `smp.c` contains no MMU or cache code; the core's exception level and MMU state come from m1n1's `start.S` *(not read)*.
- **Stopping cores.** `smp_stop_cpu` writes `1 << (4*cluster + core)` to `base + 0x0`,
  then asks the core to run `cpu_sleep`.

### m1n1 → Linux (`kboot.c`)

- **Device tree.** For every `/cpus/cpu@*` node:
  - If the core is alive, m1n1 writes
    `cpu-release-addr = smp_get_release_addr(cpu)`, which is the address of that core's
    `spin_table[i].target`. The Linux tree already says `enable-method = "spin-table"`
    with `cpu-release-addr = <0 0>`, "to be filled by loader".
  - If the core is not alive, m1n1 removes its node, drops it from the AIC
    `affinities` and `cpu-map`, and renumbers the clusters and cores.
- m1n1 makes no MMU or exception-level change to the secondaries before the kernel runs (verified
  in the part of kboot.c that was read). It switches the
  secondaries to WFE mode (`smp_set_wfe_mode(true)`) so that Linux's SEV can
  wake them *(unverified: that part of kboot.c was only summarized)*.
- On M4 and later (Apple system registers locked), m1n1 adds `idle=nop arm64.nowfxt` to
  bootargs, because the cores lose state in WFI.

### Linux releases them (`smp_spin_table.c`)

1. `cpu_init` reads `cpu-release-addr` from the device tree.
2. `cpu_prepare` maps the release address cached (`ioremap_cache`) and writes the physical address of
   `secondary_holding_pen` there. It then cleans and invalidates the line to the point of coherency (`dcache_clean_inval_poc`)
   and sends `sev`.
3. `cpu_boot` writes the core's logical ID to
   `secondary_holding_pen_release` (in `.mmuoff.data.read`: the pen polls it
   with the MMU off), cleans it to PoC, and sends `sev` again.

### IPIs and timers (`irq-apple-aic.c`)

- **IPIs ("fast IPI").** On `apple,t8103-aic` and `apple,aic2` (the M1 Pro), IPIs bypass
  the AIC and use system registers:
  - Same cluster: `IPI_RR_LOCAL_EL1` (`s3_5_c15_c0_0`), holding the target's Aff0.
  - Another cluster: `IPI_RR_GLOBAL_EL1` (`s3_5_c15_c0_1`), holding Aff0 plus the cluster in bits 23:16.
  - "Same cluster" means equal MPIDR **Aff1**; Aff2 is ignored.
  - The type field allows IMMEDIATE, RETRACT, DEFERRED or NOWAKE; Linux uses IMMEDIATE.
- **Receiving.** IPIs arrive as **FIQ**. `aic_handle_fiq` checks `IPI_SR_EL1`
  (`s3_5_c15_c1_1`) and acknowledges by writing its pending bit back.
  - 32 software IPIs are multiplexed onto this one hardware IPI (`ipi_mux`).
  - Older AIC1 parts use MMIO registers instead (`AIC_IPI_SEND` 0x2008, `AIC_IPI_ACK` …).
- **Timers.** The generic timers are also **FIQ**. The handler polls `CNTP_CTL`/`CNTV_CTL`
  (and the `_EL02` versions under VHE, gated by `IMP_APL_VM_TMR_FIQ_ENA_EL2`).
- **Device interrupts (AIC2).** These are IRQs and are not routed per CPU: the driver
  has no `irq_set_affinity` for AIC2. FIQs are restricted to sets of CPUs by the
  device tree's `affinities` nodes.

### Topology and idle

- **M1 Pro MPIDRs** (`t600x-common.dtsi`):
  - E-cores `0x0`, `0x1` (Icestorm, cluster 0).
  - P-cores `0x10100–0x10103` and `0x10200–0x10203` (Firestorm, clusters 1 and 2, Aff2 = 1).
  - `capacity-dmips-mhz` is 714 (E) and 1024 (P), which is how the Linux scheduler weighs E against P.
- **Idle** (`cpuidle-apple.c`) has two states:
  - **WFI:** clock-gated.
  - **"CPU PD"** (powered down, residency ≥ 10 ms): sets bits 25:24 of the
    implementation-defined register `s3_5_c15_c5_0`, then `wfi` in a loop until `ISR_EL1` is non-zero.
  - The dtsi notes that the top P-core frequencies stay off "until CPU deep sleep is implemented".

## 2. XNU

XNU owns the cores from reset: iBoot points the reset vector at XNU, and
there is no boot-loader spin table.

### Starting a core

- **`cpu_start(cpu)`** (`osfmk/arm64/cpu.c`):
  1. Sets the target's `cpu_reset_handler = start_cpu_paddr`.
  2. Picks its first thread (startup or idle) and flushes its `cpu_data` to memory.
  3. Under CTRR, serializes the per-cluster lockdown.
  4. Calls `PE_cpu_start_internal(cpu_id, …)`, which calls `targetCPU->startCPU()`.
  - On Apple SoCs (`USE_APPLEARMSMP`), `startCPU` lives in a platform kext, not in
    `IOCPU.cpp`. Which register it pokes is not in the open sources *(unverified)*;
    it is presumably the same PMGR CPU-start block m1n1 uses.
- **Reset path** (`start.s`). `LowResetVectorBase` is page-aligned. "On reset, both
  RVBAR_EL1 and VBAR_EL1 point here", and iBoot computes its position to program
  RVBAR. `reset_vector` then:
  1. Unlocks the OS lock and masks DAIF.
  2. Reads MPIDR and looks the core up in `ResetHandlerData` (by Aff0, or Aff0|Aff1 with clusters).
  3. Jumps to its handler, which must be `start_cpu` or `resume_idle_cpu`. Anything else hangs (`b .`).
  - The per-CPU data is physically contiguous because it "is accessed during reset with the MMU off".
- **`start_cpu` → `common_start`.** Loads TCR, TTBR0 (the boot identity map),
  TTBR1 and MAIR, invalidates the TLB, sets VBAR to the real vectors, turns on the MMU and caches,
  switches TTBR1 to the core's tables, and jumps into `arm_init_cpu`.
- **Topology** (`ml_parse_cpu_topology`, `machine_routines.c`):
  - Walks ADT `/cpus` and reads each core's `reg`, `state` ("running" = boot core), `cluster-type` (`E`/`P`),
    `die-id`, `cpu-impl-reg`, `cluster-power-down`, ….
  - Logical cluster IDs come from MPIDR Aff1.
  - The `cpus=` and `cpumask=` boot arguments limit which cores are used.

### IPIs

- **Sending** (`cpu_signal_internal`, `cpu_common.c`):
  1. Sets a message bit in the target's `cpu_signal` mask with a compare-and-swap (`SIGPxcall`, `SIGPast`,
     `SIGPTimerLocal`, `SIGPnop` …).
  2. Issues `dsb ishst`.
  3. Calls `ml_cpu_signal` (or `ml_cpu_signal_deferred` when fast IPIs are on).
- **`ml_cpu_signal_type`** uses the same registers as Linux: `S3_5_C15_C0_0` (local, same Aff1)
  or `S3_5_C15_C0_1` (global, cluster in bits 23:16), with the sequence "DSB, MSR, ISB".
  - Deferred IPIs use the countdown register `S3_5_C15_C3_1`, 64 µs by default (`fastipitimeout`).
- **Receiving.** IPIs arrive as **FIQ**. `fleh_fiq` → `sleh_fiq`, which reads and
  acknowledges `S3_5_C15_C1_1` and calls `cpu_signal_handler`.
- Cross-calls panic if the target never acknowledges ("has failed to respond to cross-call").

### Timers, idle, preemption

- **Timers.** The timer is also an FIQ. `sleh_fiq` checks `ml_get_timer_pending()` first and calls
  `rtclock_intr(TRUE)`. Each CPU has its own decrementer (`cpu_decrementer`, `rtcPop`).
- **Idle** (`cpu_idle`, `machine_idle`):
  1. First WFE loops: a cluster WFE recommendation (`wfe_process_recommendation`, IRQs unmasked) and
     "Poll issuing WFEs until the expected timer FIQ arrives".
  2. Then `wfi` (or `arm64_retention_wfi` on cores with a retention state).
  - A core waking from deep idle comes back through the reset vector with
    `resume_idle_cpu`.
- **Preemption** is AST-based and involuntary:
  - `fleh_irq` and `fleh_fiq` return through `exception_return_dispatch`.
  - `return_to_kernel` calls `ast_taken_kernel` when `AST_URGENT` is set; the return to user calls `ast_taken_user`.
  - Schedulers return `AST_PREEMPT` (plus `AST_URGENT`) when a higher-priority thread is waiting.
  - Remote cores are told to look via `SIGPast`.
  - Quantum: 10 ms by default (`DEFAULT_PREEMPTION_RATE` 100). Clutch buckets range from 10 ms (FG) to 2 ms (BG).
  - The timer → quantum-expiry link (`rtclock_intr` → `sched_default_quantum_expire`) is *(unverified)*.
- **Schedulers.** `sched_amp.c` ("amp") is compiled when `__AMP__ && !CONFIG_SCHED_EDGE`.
  `sched_clutch.c` holds Clutch and the **Edge** scheduler.
  - **AMP:** a thread is "P-recommended" or not. P-recommended threads may run on E or P, E-recommended threads
    only on E, and E-cores steal from the P set under load.
  - **Edge:** each thread has a preferred cluster; buckets on a cluster of the wrong type
    ("foreign") are rebalanced home.
  - Which scheduler a given Apple Silicon kernel config selects is *(unverified)*.

## 3. TamaGo

TamaGo has no kernel. "SMP" means letting the Go runtime run Ms on more
than one core, through hooks in `runtime/goos`:

| Hook | Called by | Meaning |
|---|---|---|
| `goos.Task(sp, mp, gp, fn)` | `newosproc` (a new M) | run `fn` (= `mstart`) on another core with stack `sp` and `g0` = `gp` |
| `goos.ProcID()` | `minit` | this core's ID, saved as `m.procid` |
| `goos.Wake(procid)` | `semawakeup` | wake a core halted in `Idle` |
| `goos.Idle(until)` | `beforeIdle`, the scheduler's idle path | halt until `until` (0 = no timer) |

- **arm64:** TamaGo ships **no** secondary-core start-up. A board must provide
  all of it: power-on, entry code, MMU and vectors, and the three hooks. godeep's
  `board/virt` and `board/applem1` do this.
- **amd64** (`amd64/smp.go`, `CPU.InitSMP`):
  1. Copies a real-mode trampoline to 0x4000 and builds a GDT and CR3 for it.
  2. Sends each AP an INIT IPI, waits 10 ms, then a SIPI with vector `0x4000 >> 12`.
  3. The APs reach `apstart` and halt waiting for an NMI.
  4. `GOMAXPROCS(n)` waits up to 1 s for the n-1 APs, then sets `goos.ProcID`/`Task`/`Wake`.
  5. `Task` writes `{sp, g, pc}` to 0x6020 and sends the next AP an NMI.
  6. `Wake` sends a LAPIC IPI with `IRQ_WAKEUP`.
- **Limits that shape everything above** (also in CLAUDE.md):
  - **Ms are never dropped.** `beforeIdle` always returns `otherReady = true`
    ("to ensure that no M is ever dropped"), so `Task` runs once per extra core
    (GOMAXPROCS-1 times). Idle Ms keep their P and poll instead of parking. As a
    result, `wakep` rarely finds an idle P, `goos.Wake` is seldom called, and a core
    finds new work only when its own idle sleep ends.
  - **No preemption.** `preemptMSupported = false` and `preemptM` is empty, and there
    is no sysmon tick. A goroutine that never blocks or allocates keeps its core
    (and the timers queued on its P) indefinitely.
  - **No timer interrupt in the scheduler.** The Go timer heap is checked only when the scheduler
    runs; `Idle(until)` is the only place a deadline meets the hardware.

## 4. godeep, in this light

godeep combines the pieces:

- **Starting cores.** On the Mac, godeep relies on m1n1 to do Asahi's start-up and
  releases cores through the spin table like Linux. On QEMU it uses PSCI.
  It then hands each core to the Go runtime with TamaGo's `goos` hooks.
- **Wake-ups.** godeep uses WFE + SEV with a per-core flag, not fast IPIs. It needs no FIQ handling,
  at the cost of waking every core on each `Wake`. The ~0.7 ms event stream bounds
  how long a missed wake-up can last.

Differences that matter for the 2026-11-27 native hang (`mac-smp-boot-hang`):

1. **The released core arrives by a C call, not a fresh entry.** The core runs `target(args…)` from
   m1n1's `smp_secondary_entry`, on m1n1's stack, at m1n1's exception level and with m1n1's
   EL2 translation state. It is not a reset-style entry. `secondaryEntry`'s EL2 part touches no memory
   before it ERETs to EL1, so an EL2 MMU left on only has to map godeep's code. That holds
   if m1n1 identity-maps RAM *(unverified: m1n1 `start.S` / `mmu.c` not read)*. With E2H=1,
   m1n1's own `SCTLR_EL1` writes go to `SCTLR_EL2`, so EL1 starts with its own state.
   godeep then loads all of it (MAIR, TCR, TTBR0, VBAR, SCTLR).
2. **SEV only wakes a WFE-mode loop.** If m1n1 left the secondaries in deep-WFI mode, they wait for an IPI,
   and godeep's SEV would not wake them. Linux's spin-table boot relies on the same WFE mode, and that
   mode is set in kboot *(unverified)*. A core that never starts shows up as
   "didn't start" after 1 s; it is not a hang.
3. **Cluster numbering.** Linux and XNU take the cluster from MPIDR **Aff1** (Aff2 = 1 marks P-cores
   on the M1 Pro). godeep matches whole MPIDRs (Aff0–Aff3), which is equivalent
   for finding a slot.
4. **Interrupts and preemption.** XNU and Linux preempt from the timer FIQ and send work across cores with
   IPIs. godeep has neither, so with SMP a non-yielding goroutine pins a core,
   and work queued on one core waits for another core's idle timeout.

Booting with `godeep.nosmp` separates (1)–(2) from the other changes in that
build.
