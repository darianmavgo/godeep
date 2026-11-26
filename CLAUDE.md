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
make check-virt # headless QEMU virt: PCIe + NVMe + GPT + FAT32/ext4 read from the shell (tools/qemucheck)
make qemu      # boot it with an interactive shell on serial (arm64: the Image + device tree; BOOT=elf for the ELF; EL2=1 to enter at EL2 like m1n1, TCG)
make qemu-applem1   # the Mac image itself on QEMU sbsa-ref, handed over like m1n1 (tools/fakem1n1); TCG
make shot-applem1   # (another terminal) its framebuffer → bin/fakem1n1/screen.png
make check-applem1  # headless: Mac image reaches the shell, and the fault screen works (stages 1, 3)
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
  - `dma`: DMA allocator over a region outside the Go heap + `IOMMU` interface (`Identity`; Apple DART to do). `Buffer.Load32/Store32`: acquire/release access to words a device shares (completion phase bits, ownership flags)
  - `block`: block-device interface, GPT/MBR partitions, cached `io.ReaderAt` (`NewReader`); `MakeGPT` for tools/tests. Read-only
  - `irq`: interrupt bridge. The board acks and calls `Dispatch`; the line is masked until the driver calls `line.Done()` (oneshot)
  - `dev`: device manager. Each driver is a process `drv/<name>` under `devmgr`, with dependencies (`Needs`), readiness (`Ready`/`Get`) and restart-on-crash
  - `rtkit`: Apple RTKit coprocessor protocol over a `Mailbox` interface (encodings unverified on hardware)
  - `fdt`: device-tree parser (`ranges` translation, GIC interrupt cells, phandles: `Refs` for `*-gpios`/`power-domains`/`interrupts-extended`); boards build `dev.Node`s from the tree in `x0`. `dev.Node.Config` carries board-resolved driver settings (or an error the driver returns, so lsdev shows it)
  - `mmio`: `Regs` interface for device registers: `Window` (tamago only) and a recording `Fake` for host tests
- `drivers/pci`: ECAM host bridge (`pci-host-ecam-generic`): scans bus 0, assigns BARs from `ranges` (no firmware does it), adds each function as a device named by class (`nvme0`) with compatible `pciclass,CCSSPP` and Config `*pci.Function` (`dev.Device.Add`: drivers can add devices they discover)
- `drivers/nvme`: NVMe over PCIe, polled, read-only; one goroutine owns the controller, `ReadBlocks` requests go over a channel; publishes a `block.Device`
- `fs/fat`, `fs/ext4`: read-only filesystems as `io/fs.FS` (FAT12/16/32 + long names; ext2/3/4: extents, block maps, 64-bit, htree, symlinks), over the shared `fs/rofs`. Tested against real `mkfs` images in `fs/testdata` (`tools/mktestfs.sh`, needs `brew install e2fsprogs dosfstools mtools`) and `testing/fstest`. Shell: `lsblk`, `ls DISK:PATH`, `cat DISK:PATH` (no mount table: `nvme0p2:/docs`)
- `tools/mkdisk`: `bin/disk.img` (GPT: FAT32 + ext4 test images), QEMU's NVMe disk. `tools/qemucheck`: boot QEMU, type commands, expect output
- `drivers/<vendor>/<device>`: portable drivers taking `mmio.Regs`, host-tested; boards register them (`drivers/arm/pl061`, `pl031`, `drivers/virtio/mmio`, `drivers/apple/wdt`: the Apple watchdog, a `dev.Rebooter` for the shell's `reboot`; `drivers/apple/{pmgr,gpio,spi,spihid}`: the internal keyboard, polled like U-Boot's, see below)
- `tools/mkimage`: ELF → arm64 Linux Image (`-text-offset` override to test self-relocation)
- `tools/m1n1boot`: puts godeep in place of U-Boot in an Asahi `m1n1/boot.bin` (m1n1 boots godeep directly; U-Boot on this Mac autoboots Omarchy). `-info` tells which payload a boot.bin has
- `tools/fakem1n1`: m1n1 stand-in for QEMU `sbsa-ref`, whose RAM starts at 1 TB like the M1's, so `bin/godeep-applem1.img` runs unmodified. An EL3 shim (`shim.asm`, words in `shim_code.go` via `go generate`) drops to EL2 with E2H=1. It loads the Image away from its link address and passes an m1n1-style tree (`/chosen` framebuffer, `/memory`, `/reserved-memory`, Apple-style nodes, plus a PL011 so the console is mirrored to the terminal). `shot` = framebuffer PNG over QMP; `check -fault N` = fault-screen test. sbsa-ref puts its own DTB in the first MB of RAM, so the shim sits at +2 MB
- Early boot screen (`board/applem1/early.go`): numbered squares on m1n1's framebuffer for each boot stage (1 Hwinit0, 2 final page tables, 3 Hwinit1, 4 runtime up), and a red fault screen (ESR/ELR/FAR) from early vectors (`entry_arm64.s` → `early_arm64.s`), then via `arm64.SystemExceptionHandler` until the console exists. Built on `fdt.ScanEarly` and `simplefb.Early`: allocation-free, and the painter is `//go:nosplit` (it runs on a private stack). `godeep.testfault=N` in `/chosen/bootargs` faults at stage N on purpose
- `kernel/pgtable`: 4-level 48-bit page tables, allocation-free (runs in `Hwinit0`, before the Go runtime)
- `drivers/simplefb`: text console on a firmware framebuffer
- `board/applem1` (`tamago && arm64 && applem1`, `make image BOARD=applem1`): this MacBook Pro. Boots to the shell under QEMU sbsa-ref via `make check-applem1`, and on the real Mac via m1n1 (2026-11-25; no input yet). Idle = WFE woken by the generic-timer event stream (`idle.go`; no interrupts needed; under QEMU TCG WFE doesn't sleep). The watchdog (30 s) is stopped by `goos.Exit` and `lateFault` so halt/fault screens stay. fakem1n1's watchdog node is a RAM page: it is wired but never counts or resets. Keyboard: `spihid.ConfigFromFDT` resolves the transport node (power-domain chains, spi3 pinmux, spien, ready line) and the board feeds its bytes into the same console channel as autorun. It polls the ready line every 5 ms, or falls back to blind reads after 3 s; lsdev's `hid-transport@0` row reports mode, power, packets, CRC and keys. fakem1n1 backs this path with preloaded RAM pages (`keyboard.go`, `regs.bin`): it exercises bring-up and the fallback, not typing. On QEMU you type through the PL011 instead (`drivers/arm/pl011`, polled, same input channel); `check-applem1` types `help` and expects the shell's answer See docs/TargetMyMac.md §3.1, §6.8
- `third_party/tamago`: vendored TamaGo, via `replace` in go.mod: arm64 `linkhwinit0` (2 files), and `goos.HeapAddrBits` 41 instead of 40 (M1 DRAM starts at 1 TB = 1<<40; with 40 the runtime aborts in `schedinit`). See its GODEEP_PATCHES.md
- `tools/godeep-partition.sh`: guided partitioning of this Mac (docs/TargetMyMac.md §6)
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
- Entry code (`entry_arm64.s`) must stay position-independent until relocation: Go assembles `MOVD $sym(SB)` as PC-relative (ADRP+ADD), so keep absolute addresses in DATA words. No Go code before the early identity map: with the MMU off, Go's vector loads/stores fault on Device memory.
- `IMAGE_BASE` in each board's entry_arm64.s must equal the Makefile's `TEXT_START - 0x1000`.
- **QEMU 11.1.x HVF WFI bug:** after the first device (SPI) interrupt, WFI returns immediately and the idle VM spins at 100%. TCG is fine and the guest state is clean. Under HVF (`MIDR` implementer `0x61`) the UART is polled (`UARTMode`) to avoid it. To measure idle cost: `ps -o %cpu= -p $(cat bin/qemu-arm64.pid)`. Debug counters must not add Go timers or keystrokes, since both change idle behaviour.
- **Power button:** QEMU `system_powerdown` (sent by `make stop` over QMP) raises PL061 GPIO line 3 (`0x09030000`). `board/virt/power.go` polls it, and `kernel.Boot` then kills every process (`Table.Shutdown`, 2 s grace) and calls PSCI off. This works with the default `-machine virt` (no `acpi=off` needed when booting with `-kernel`). amd64 has no power-button handler, so `make stop` falls back to SIGTERM there.

amd64 / `board/microvm`:
- **QEMU needs `-m 3G` or more.** The microvm board maps RAM up to `0xb0000000`. With less memory, the stack lands in unbacked RAM and the guest silently triple-faults (jump to RIP=0).
- macOS has no KVM, so QEMU runs under TCG (`-cpu max`). tamago detects TCG and calibrates the TSC via the LAPIC timer. Don't use `kvmclock`/`-enable-kvm` flags there.
- Don't build with the `linkhwinit0` tag (tamago-example uses it): it removes the default `Hwinit0`, and the link fails unless we provide our own.
- To debug boot crashes, add `-d int,cpu_reset` to QEMU and look for `Triple fault` and the RIP/SP before it.

- **Kill is cooperative.** Go cannot preempt-kill a goroutine. A cancelled Proc dies at its next blocking kernel call.
- TamaGo has no cgo, no `plugin` and no `os/exec`. Anything dynamic goes through the (planned) wazero loader.
- Single address space means trust: only memory-safe Go (no `unsafe` outside `board/*`, `kernel/dev` and `kernel/dma`'s word accessors).
