# Understanding goroutine failure in godeep

godeep runs every process as goroutines in one address space, with Go's
memory safety standing in for the MMU. This note answers one question: **when
one goroutine fails, can the others keep running?** Partly. Which failures
stay contained depends on how the goroutine fails.

Status as of 2026-11-28. Items marked **fixed** changed that night; see
"Changes" at the end.

## Contained: the process dies, the kernel keeps running

| Failure | What happens | Where |
|---|---|---|
| Go `panic` in a Proc goroutine | `p.Go` / the Proc's main recover it, record `panic: …` as the Proc's error, cancel the Proc. Other Procs are unaffected | `kernel/proc/proc.go` (`run`) |
| A driver's `Run` returns an error or panics | The device manager restarts it (`lsdev`: state `restarting`, `RESTARTS` count, the error in INFO) | `kernel/dev` |
| **CPU fault in Go code** (nil pointer, bad address, misalignment, undefined instruction) | **fixed:** becomes `runtime error: invalid memory address or nil pointer dereference` (a normal `runtime.Error` panic) in the faulting goroutine, as on hosted Go; a `recover` can stop it. Any number per boot | runtime patch `sigpanic_tamago*`, `third_party/tamago/arm64/fault_godeep.s`, `arm64.FaultPanics` |
| A program misusing what it wasn't given | Can't: authority only comes from capabilities passed in | `kernel/cap`, `restrict` |

Try it: `crash panic|nil|write|leaf|frame|recover|storm|child` (coreutils),
and `make check-crash`.

## Not contained: these take the whole machine down

### 1. Fatal runtime errors (`throw`)

Concurrent map writes, out of memory, stack exhaustion, "all goroutines are
asleep", corrupted runtime state, and a fault *inside the runtime* (in the
allocator, with a runtime lock held, on the system stack) are `throw`s, not
panics. `recover` can't catch them, by design: the runtime's state may be
inconsistent. That's Go, not TamaGo.

Mitigation: avoid them (the race detector finds the map races: tests run
with `-race -count=50`), and keep `proc.Limits` on memory-hungry programs.

### 2. Hangs: a goroutine that never yields

TamaGo has **no preemption** (no sysmon, no async preemption signals). A
goroutine that loops without blocking or allocating keeps its core forever,
along with the timers queued on that core's P. Kill is cooperative too (a
cancelled Proc dies at its next blocking call), so nothing can stop it.

- 1 core: the whole kernel freezes.
- SMP: one core is lost; the rest keep going, minus that P's timers.

Not every hang is a spinning goroutine. On QEMU HVF the whole kernel used
to stall for 15 s or more about 1 boot in 10: `board/virt`'s idle loop
armed the virtual timer for a deadline only a few ticks away, the timer had
already fired when WFI ran, and HVF didn't wake the core for it. Every Go
timer waited for the next unrelated interrupt. **Fixed** (`minWFI`: no WFI
for deadlines under 50 µs). The tools that found it are worth reusing:
goroutines dumped from the power-button handler (runnable goroutines, the
only M idle), then the longest single WFI (15 s, armed for 3 ticks).

Mitigations: CPU-bound loops call `runtime.Gosched()` (as `burn` does).
Programs that can't be trusted to do that run as WASM (below). The Mac's
hardware watchdog (30 s) resets a frozen kernel.

### 3. Asynchronous hardware errors (SError)

Some bad device accesses (a powered-off block on the Mac, a bus error) come
back as an *asynchronous* SError, not a fault at the instruction. It can't
be tied to a goroutine, so it stays fatal: the Mac paints the red fault
screen, QEMU prints the registers.

### 4. Memory corruption

There's one address space and no per-process page tables. Anything that
writes memory outside Go's type system can corrupt anyone:

- `unsafe` / MMIO code (allowed only in `board/*`, `kernel/dev`,
  `kernel/mmio`, `kernel/dma`'s accessors);
- devices doing DMA (NVMe, xHCI, the Mac's DWC3, ANS2, AGX firmware) given a
  wrong address, or using a buffer after it was freed;
- a secondary core started with bad state (SMP bring-up).

Mitigation: an IOMMU (Apple DART) would confine each device's DMA to its
buffers. `kernel/dma` has the `IOMMU` interface; the DART driver is to do.

### 5. Goroutines started by libraries

gVisor (netstack) and `x/crypto/ssh` start their own goroutines with a bare
`go`. A panic there is outside any Proc's `recover` and stops the kernel.
Mitigations already in place: call them from a Proc, fuzz untrusted input
(`programs/sshd` `FuzzPreAuth`).

## How a CPU fault becomes a panic (the fix)

Hosted Go turns a fault into a panic through a signal: the kernel delivers
SIGSEGV, and the runtime's handler (`sigctxt.preparePanic`) makes it look
as if the faulting instruction had called `runtime.sigpanic`. TamaGo has no
signals. Its exception vector jumped into Go and called `panic` from a frame
the unwinder didn't recognise, left IRQs masked on that core, and set a flag
so that the *second* fault in a boot halted the machine. On `board/virt`, a
nil pointer read didn't even fault: page 0 was mapped (QEMU's flash), so it
returned garbage.

Now:

1. **Page 0 faults.** Both boards leave the first 64 KB unmapped
   (`pgtable.NilGuard`). Go's nil checks are loads that must fault: the
   compiler drops explicit checks for offsets below 4 KB.
2. **The vector hands synchronous faults to the runtime.**
   `third_party/tamago/arm64/exception.s` calls `faultPanic`
   (`fault_godeep.s`). If the board set `arm64.FaultPanics`, there is a g,
   and ESR says data/instruction abort, alignment or undefined instruction,
   it jumps to `runtime·tamagoFault`. Anything else (SError, no g) goes to
   `SystemExceptionHandler` as before (the Mac's fault screen).
3. **`runtime·tamagoFault`** (godeep's runtime patch) stores ESR/FAR/ELR in
   `g.sigcode0/sigcode1/sigpc` and restores the interrupted code's DAIF.
   It then does what `preparePanic` does: saves LR on the stack so leaf
   functions unwind, sets LR to the faulting PC, and branches to
   `runtime.sigpanic`. The unwinder knows that frame (`FuncID_sigpanic`):
   it doesn't look up liveness at the faulting PC, and it resumes a
   recovering frame at its deferreturn.
4. **`runtime.sigpanic`** (tamago/arm64) throws if the fault hit the runtime
   (`canpanic`), and otherwise panics with the same errors hosted Go uses.

`board/virt` turns it on in `Hwinit1`, `board/applem1` once its console
exists (before that, the fault screen is the better report). It needs the
patched toolchain (`make tamago-go`; the Makefile adds the `godeepruntime`
tag only when the toolchain has the files). With the stock toolchain,
faults behave as before.

Tested (QEMU virt, HVF and TCG): `make check-crash` runs every kind of fault
in a row, including leaf functions with and without frames, a nil write,
and a fault in a child goroutine. It also checks a program that recovers
its own fault and exits 0, and 8 goroutines × 500 faults recovered with
`runtime.GC()` running (on 1 and 4 cores). Timers and the shell work
afterwards.

## What else could be done, cheapest first

1. **Defer the risky bring-up on the Mac** until the shell and boot log are
   on screen: releasing the other cores (`startSMP`) and starting `gpu0`
   (AGX). Their failures (memory corruption, SError) stay machine-wide, but
   they'd happen with the shell and the whole boot log visible.
2. **A stall detector.** A kernel goroutine bumps a counter every 100 ms. On
   board/virt the timer interrupt's top half (`irqTop`, which runs in the
   exception) could notice the counter stopped and print the interrupted PC,
   naming the goroutine that spins. On the Mac this needs AIC timer
   interrupts (the board polls today).
3. **Preemption.** A per-core timer interrupt that asks the running
   goroutine to yield (Go's async preemption, injected like sigpanic above).
   That fixes hangs at the root, but secondary cores run with IRQs masked
   today and the runtime's async-preemption path has never run on TamaGo.
4. **IOMMU (DART)** for every DMA-capable device on the Mac.
5. **WASM for untrusted programs** (`programs/wasm`, already there): linear
   memory is bounded (`proc.Limits.Memory`), and the interpreter can be
   made to check the context, so a spinning WASM program can be killed.
6. **Hardware isolation per process** (own page tables, or a core and
   memory of its own). That's a different design from godeep's single
   address space, and it's what the language-based approach is meant to
   avoid.

## Changes (2026-11-28)

- `kernel/pgtable.NilGuard`, used by `board/virt` and `board/applem1`: page 0
  unmapped, so nil pointers fault (on virt they returned flash contents).
- Runtime patch: `runtime.sigpanic` and `runtime·tamagoFault` for
  tamago/arm64 (`third_party/tamago-go/godeep-runtime.patch`, README).
- Vendored TamaGo: `arm64.FaultPanics`, `fault_godeep.s` / `fault_stock.s`
  (GODEEP_PATCHES.md item 5).
- Makefile: `godeepruntime` tag when the toolchain has the patch;
  `make check-crash` (also run by `check-virt`).
- coreutils `crash`: faults on purpose.
- `board/virt/idle.go` `minWFI`: the HVF timer stall above (check-linklocal
  hung 1-3 runs in 10-12, stock TamaGo too; 0 in 30 after).

Verified 2026-11-28: `make test`, the full `make check-virt` (209 steps,
check-crash included), `make check-applem1`, `crash nil` on the Mac image
under fakem1n1, `make check-parallels`, and the crash steps (storm too) on
Parallels with 4 cores.
