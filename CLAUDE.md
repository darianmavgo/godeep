# godeep

godeep is a kernel and OS written in Go where **Go's concurrency model is the process model**. It has no separate address spaces and no OS context switches:

| Classic OS | godeep |
|---|---|
| Process | `proc.Proc`: PID, root `context.Context`, a supervised goroutine group, capability table, stdio channels, accounting |
| fork/exec | `proc.Spawn(ctx, prog, args, caps)` |
| kill / signals | context cancel + `Signals` channel (cooperative, see Constraints) |
| Scheduler | the Go runtime scheduler (GMP); the kernel only adds policy and accounting |
| Syscalls | ordinary method calls on capability interfaces (`cap.File`, `cap.Net`, …) |
| IPC / pipes | typed channels; `ipc.Port` for named request/reply services |
| File descriptors | capability handles (unforgeable Go values that are passed in) |
| Drivers | one goroutine owns each device; IRQs are delivered as channel sends |
| Programs | compiled into the image and registered via `init()`; later, WASM via wazero |

Isolation comes from the language: everything runs in one address space, and Go's memory safety stands in for the MMU. The kernel runs bare-metal on **TamaGo** (`GOOS=tamago`), with two board targets:
- **arm64 on QEMU `virt` with HVF acceleration** (the default; native speed on Apple Silicon)
- **amd64 on QEMU `microvm`** (emulated on Apple Silicon)

The same core also builds as a **hosted** binary for macOS/Linux, which is used for fast development and tests.

Full roadmap: `docs/architecture.md`.

## Toolchain setup (one time)

- Host Go: `brew install go`
- TamaGo Go (no macOS prebuilt, so build it from source with the host Go). Keep the tag matching the `github.com/usbarmory/tamago` version in go.mod:
  `git clone --depth 1 --branch tamago-go1.27.4 https://github.com/usbarmory/tamago-go ~/tamago-go && (cd ~/tamago-go/src && ./make.bash)`
- QEMU: `brew install qemu`

## Build

```
make test      # go test ./... on the host (core packages must stay host-buildable)
make hosted    # build ./bin/godeep-hosted and run the kernel as a normal process
make kernel    # build bin/godeep-$(ARCH).elf with TamaGo (TAMAGO=path/to/tamago-go/bin/go)
make image     # arm64 Linux-style Image (bin/godeep-arm64.img) for QEMU -kernel, m1n1, U-Boot booti
make qemu      # boot it with an interactive shell on serial (arm64: the Image + device tree; BOOT=elf for the ELF; EL2=1 to enter at EL2 like m1n1, TCG)
make stop      # (another terminal) power button → clean shutdown; falls back to SIGTERM/SIGKILL
make kill      # (another terminal) SIGKILL QEMU
```

`ARCH` selects the target:

| ARCH | Board | QEMU | Speed on Apple Silicon |
|---|---|---|---|
| `arm64` (default) | `board/virt` | `qemu-system-aarch64 -machine virt,gic-version=3 -accel hvf -cpu host` | native (HVF) |
| `amd64` | `board/microvm` | `qemu-system-x86_64 -machine microvm` | emulated (TCG) |

`make qemu ACCEL=tcg` runs the arm64 kernel without HVF (with `-cpu max`), e.g. on Linux/x86 hosts.

In QEMU, ^C goes to godeep (it kills the foreground program). Quit QEMU with `Ctrl-A` then `X`, or type `halt`. On arm64, `halt` calls PSCI `SYSTEM_OFF`; on amd64 the guest triple-faults and `-no-reboot` exits.

## Layout

- `cmd/godeep/`: bare-metal entry point (`//go:build tamago`)
- `cmd/godeep-hosted/`: host entry point
- `kernel/…`: portable core. **No board imports.**
  - `proc`: process table. Each Proc tags its goroutines with pprof labels `pid`/`proc`; children inherit them. `Snapshot()` parses `runtime.Stack(all)`, which in Go 1.27 prints labels in each goroutine header, to attribute every goroutine to a PID (this is what `ps` uses). `Kthread` = kernel goroutine (pid 0).
  - `tty`: line discipline, run as its own kernel goroutine: echo, editing, typeahead, ^C → `Interrupts()` channel
  - `prog`: registry of built-in programs (registered from `init()`, read-only after boot)
  - `dma`: DMA allocator over a region outside the Go heap + `IOMMU` interface (`Identity`; Apple DART to do)
  - `irq`: interrupt bridge. The board acks and calls `Dispatch`; the line is masked until the driver calls `line.Done()` (oneshot)
  - `dev`: device manager. Each driver is a process `drv/<name>` under `devmgr`, with dependencies (`Needs`), readiness (`Ready`/`Get`) and restart-on-crash
  - `rtkit`: Apple RTKit coprocessor protocol over a `Mailbox` interface (encodings unverified on hardware)
  - `fdt`: device-tree parser (`ranges` translation, GIC interrupt cells); boards build `dev.Node`s from the tree in `x0`
  - `mmio`: `Regs` interface for device registers: `Window` (tamago only) and a recording `Fake` for host tests
- `drivers/<vendor>/<device>`: portable drivers taking `mmio.Regs`, host-tested; boards register them (`drivers/arm/pl061`, `pl031`, `drivers/virtio/mmio`)
- `tools/mkimage`: ELF → arm64 Linux Image
  - See `docs/DriverModel.md` for the driver rules and these four pieces
  - planned: cap, ipc, vfs, klog
- `board/*`: the only places with platform code, behind build tags
  - `board/virt` (`tamago && arm64`; entry code needs the `linkcpuinit` tag, set in the Makefile): **written by us**. `entry_arm64.s` replaces TamaGo's `cpuinit`: EL2 → EL1, and it copies the boot device tree to the start of the DMA region (2 MB reserved) before the Go heap exists. Devices come from that tree (`devices.go`), or a static list for ELF boots. We wrote this board because TamaGo has no arm64 QEMU-virt board. It provides the TamaGo runtime hooks via `//go:linkname`: `RamStart`/`RamSize`/`RamStackOffset`, `Hwinit1`, `Nanotime`, `Printk`, `InitRNG`/`GetRandomData`. It also has a PL011 UART driver, a GICv3 + virtual-timer idle governor, and PSCI power-off.
  - `board/microvm` (`tamago && amd64`): wraps TamaGo's `board/qemu/microvm`
  - `board/hosted` (`!tamago`)
  - `cmd/godeep/board_<arch>.go` picks the board for each GOARCH
- `programs/…`: `initd` (PID 1, respawns sh), `sh` (shell + builtins `ps`, `kill`, `mem`…), `coreutils` (demo programs)

## Rules

- Every kernel API takes `context.Context` first and **must return promptly when it is cancelled**. This is what makes kill work.
- Never use a bare `go` statement in kernel or program code. Start goroutines with `p.Go(fn)` so they are accounted to a Proc and their panics are contained (a panic kills the proc, not the kernel).
- There are no global mutable namespaces. Authority comes only from capabilities passed to a Proc.
- Prefer channels and ownership (one goroutine owns the state) over shared memory with mutexes.
- Platform-specific code lives only in `board/*`, behind `//go:build tamago` / `!tamago`.
- New core code needs host unit tests, run repeatedly with the race detector (`go test -race -count=50`). Repetition found most of the bugs so far.
- Drivers: one goroutine owns the device; register with `dev.Register` from `init()`; implement `Run(ctx, d)`; call `d.Ready(svc)`; service interrupts with `<-line.C` then `line.Done()`.
- Device registers: single 32-bit `atomic.Load/StoreUint32` accesses (HVF can't decode paired MMIO). Never read-modify-write a write-1-to-set/clear register.

## Constraints to remember

arm64 / `board/virt`:
- **Idle must sleep, not spin.** TamaGo's default arm64 idle only `WFI`s when no timer is pending, so with any sleeping goroutine it burns 100% of a host core. `board/virt/idle.go` arms the virtual timer (`CNTV_TVAL`) for the next deadline and runs `WFI` with interrupts masked in PSTATE. Under HVF, QEMU sleeps the vCPU until `CNTV_CVAL`; under TCG the timer PPI (INTID 27) wakes the core through the GIC. This needs `gic-version=3`. Measured idle cost: about 1.5% of a host core.
- **The RNG is seeded from the boot-time counter: not secure entropy.** Add virtio-rng before trusting `crypto/rand`.
- **Device access under HVF must be single 32/64-bit loads/stores** (e.g. `atomic.Load/StoreUint32`). Paired or vector accesses to MMIO can't be decoded by the hypervisor.
- TamaGo has no arm64 SMP bring-up, so it's `-smp 1`, and `GOMAXPROCS` is 1.
- Memory: Go heap = first 3.5 GB (`RamSize`); DMA region = last 512 MB of `-m 4G` at `0x1_2000_0000`, mapped as normal memory in `Init`.
- Don't use TamaGo's `GIC.DisableInterrupt`: it read-modify-writes `GICD_ICENABLER` and disables the whole bank. `board/virt` writes the single bit.
- `goos.Idle(until)`: `until == 0` means "no timer", not "due now".
- **QEMU 11.1.x HVF WFI bug:** after the first device (SPI) interrupt, WFI returns immediately and the idle VM spins at 100%. TCG is fine and the guest state is clean. Under HVF (`MIDR` implementer `0x61`) the UART is polled (`UARTMode`) to avoid it. To measure idle cost: `ps -o %cpu= -p $(cat bin/qemu-arm64.pid)`. Debug counters must not add Go timers or keystrokes, since both change idle behaviour.
- **Power button:** QEMU `system_powerdown` (sent by `make stop` over QMP) raises PL061 GPIO line 3 (`0x09030000`). `board/virt/power.go` polls it, and `kernel.Boot` then kills every process (`Table.Shutdown`, 2 s grace) and calls PSCI off. This works with the default `-machine virt` (no `acpi=off` needed when booting with `-kernel`). amd64 has no power-button handler, so `make stop` falls back to SIGTERM there.

amd64 / `board/microvm`:
- **QEMU needs `-m 3G` or more.** The microvm board maps RAM up to `0xb0000000`. With less memory, the stack lands in unbacked RAM and the guest silently triple-faults (jump to RIP=0).
- macOS has no KVM, so QEMU runs under TCG (`-cpu max`). tamago detects TCG and calibrates the TSC via the LAPIC timer. Don't use `kvmclock`/`-enable-kvm` flags there.
- Don't build with the `linkhwinit0` tag (tamago-example uses it): it removes the default `Hwinit0`, and the link fails unless we provide our own.
- To debug boot crashes, add `-d int,cpu_reset` to QEMU and look for `Triple fault` and the RIP/SP before it.

- **Kill is cooperative.** Go cannot preempt-kill a goroutine. A cancelled Proc dies at its next blocking kernel call.
- TamaGo has no cgo, no `plugin` and no `os/exec`. Anything dynamic goes through the (planned) wazero loader.
- Single address space means trust: only memory-safe Go (no `unsafe` outside `board/*` and `kernel/dev`).
