# GEMINI.md — godeep

`godeep` is a bare-metal kernel and OS written in Go where **Go's concurrency model is the process model**. Everything runs in a single address space with no OS context switches; Go's memory safety and capability passing replace the MMU and file descriptor tables.

| Classic OS | godeep |
|---|---|
| Process | `proc.Proc`: PID, root `context.Context`, supervised goroutine group, capabilities, stdio, accounting |
| `fork`/`exec` | `proc.Spawn(ctx, parentPID, spec)` |
| `kill` / signals | Context cancellation (cooperative; see Constraints) |
| Scheduler | Go runtime scheduler (GMP); kernel adds accounting via pprof goroutine labels (`pid`/`proc`) |
| Syscalls | Ordinary Go method calls on capability interfaces (`cap.Net`, `vfs.NS`, …) |
| IPC / pipes | Go channels |
| Drivers | Supervised `drv/<name>` processes under `devmgr`; one goroutine owns each device; IRQs delivered via channels |
| Programs | Built-in via `prog.Register` in `init()`, plus dynamic WASI preview 1 `.wasm` binaries via `wazero` |

The same portable core builds for:
- **Bare metal (`GOOS=tamago`)** via vendored [TamaGo](third_party/tamago/GODEEP_PATCHES.md):
  - `board/virt` (`arm64`, default): QEMU `virt` with HVF acceleration on Apple Silicon (or `ACCEL=tcg` elsewhere)
  - `board/applem1` (`arm64` + `applem1` tag): Apple M1 Pro MacBook Pro (`J316sAP`), booted via m1n1 and rehearsed on QEMU `sbsa-ref` (`tools/fakem1n1`)
  - `board/microvm` (`amd64`): QEMU `microvm` (emulated via TCG on Apple Silicon)
- **Hosted (`!tamago`)**: `board/hosted` running as a normal macOS/Linux process for fast development and unit testing.

---

## Toolchain & Commands

### One-time Setup
- **Host Go**: `brew install go` (matches `go.mod`: Go 1.27.x)
- **TamaGo Go**: build from source matching `github.com/usbarmory/tamago` in `go.mod` (`v1.27.4`):
  ```sh
  git clone --depth 1 --branch tamago-go1.27.4 https://github.com/usbarmory/tamago-go ~/tamago-go
  (cd ~/tamago-go/src && ./make.bash)
  ```
- **QEMU**: `brew install qemu`
- **Git hooks** (enforces append-only `docs/TargetMyMacProgress.log`):
  ```sh
  git config core.hooksPath tools/githooks
  ```
- **Optional (filesystem test images)**: `brew install e2fsprogs dosfstools mtools` for `tools/mktestfs.sh`.

### Build, Test & Run
Override defaults with `ARCH=arm64|amd64`, `BOARD=virt|applem1`, `ACCEL=hvf|tcg`, `TAMAGO=/path/to/go`, `SSH_PORT=2222`, `QEMU_DISPLAY=cocoa`.

```sh
make test            # go vet -stdmethods=false ./... && go test ./... (host-buildable core)
go test -race -count=50 ./<pkg>   # required stress check for new/modified kernel & driver code
make hosted          # build & run ./bin/godeep-hosted (sshd on 127.0.0.1:2222; Ctrl-D exits)
make kernel          # build bin/godeep-$(ARCH).elf with TamaGo (depends on make keys)
make image           # build arm64 Linux-style Image (bin/godeep-$(ARCH).img) via tools/mkimage
make qemu            # boot QEMU (arm64 Image + DTB on virt/HVF; BOOT=elf for ELF; EL2=1 for EL2/TCG)
make hot             # auto-rebuild & cleanly reboot QEMU on .go/.s changes (Ctrl-A x quits, Ctrl-A r reboots)
make check-virt      # headless QEMU virt integration suite (NVMe, FAT/ext4, USB kbd/mouse/hub/storage, ramfb, rng, net, SSH/SFTP, WASM, 9P, pprof, usbnet)
make check-usbnet    # headless QEMU virt test using USB CDC-ECM (usb-net) as sole NIC
make qemu-applem1    # boot bin/godeep-applem1.img on QEMU sbsa-ref via tools/fakem1n1 (TCG)
make shot-applem1    # capture fakem1n1 framebuffer to bin/fakem1n1/screen.png (from another terminal)
make check-applem1   # headless Mac image boot check + fault-screen checks (stages 1 and 3)
make native          # host tests + Mac image + check-applem1 + install to Mac ESP (DRY=1 to preview)
make wasm            # build examples/wasm/* for WASI preview 1 → bin/wasm/*.wasm
make push            # scp bin/wasm/$(WASM).wasm (default: hello) into godeep ram:/
make keys            # generate/copy dev SSH keys into programs/sshd/keys/ (gitignored)
make ssh             # ssh into godeep on 127.0.0.1:$(SSH_PORT) (supports CMD='...')
make stop            # clean QMP power-button shutdown (falls back to SIGTERM/SIGKILL)
make kill            # immediate SIGKILL of QEMU
make clean           # remove bin/
```

In QEMU: `^C` goes to godeep (kills foreground program); quit QEMU with `Ctrl-A` then `X`, or run `halt` inside godeep.

---

## Repository Layout

- `cmd/godeep/`: bare-metal entry (`//go:build tamago`) with `board_<arch>.go` / `board_applem1.go` selectors.
- `cmd/godeep-hosted/`: hosted entry (`!tamago`).
- `kernel/`: **portable core — must never import `board/*` and must stay host-buildable**.
  - `kernel.go`, `net.go`, `inputd.go`: boot orchestration (`kernel.Boot`), `netd` (DHCP + renewal + fallback, follows NIC across driver restarts), `inputd` (fans input events to `Console.Input`).
  - `proc/`: process table, `Spawn`, `Kill`, `p.Go` (panic containment), `Kthread`, `Snapshot()` (parses Go 1.27 `runtime.Stack` pprof labels for `ps`).
  - `tty/`: console line discipline (`New` as Kthread, `NewFor` scoped to a `Proc` for SSH), echo, cursor editing, 100-line history, typeahead, `^C` → `Interrupts()`.
  - `prog/`: built-in program registry (`prog.Register` from `init()`).
  - `dev/`: device manager (`devmgr`, supervised `drv/<name>` processes, `Needs` ordering, `Ready`/`Get`, backoff restart up to `MaxRestarts`, dynamic `d.Add` for bus drivers, `Node.Config`).
  - `irq/`: oneshot interrupt bridge (`Dispatch` masks until `line.Done()`), stats, latency probe support.
  - `dma/`: allocator outside Go heap (128 B min alignment, 16 KB for Apple DART), `IOMMU` interface (`Identity` now; DART planned), `Load32`/`Store32` acquire/release accessors.
  - `mmio/`: `Regs` / `ByteRegs` interfaces (`Window` on tamago, recording `Fake` for host tests).
  - `fdt/`: device-tree parser (`ranges`, GIC interrupt cells, phandles via `Refs`/`ByPhandle`, allocation-free `ScanEarly`).
  - `pgtable/`: allocation-free 4-level 48-bit ARM64 page tables (runs in `Hwinit0` before Go runtime).
  - `block/`: `block.Device`, `block.Writer`, GPT/MBR partitioning (`gptwrite.go`), cached `io.ReaderAt`/`io.WriterAt`.
  - `vfs/`: per-Proc volume namespace (`VOLUME:PATH` in shell, `/VOLUME/PATH` in SFTP): `ram:` (`memfs`), named device volumes (`host:` via 9P), block disks/partitions (auto-detects FAT/ext4, writable when supported).
  - `cap/`: capability interfaces (`cap.Net`, `cap.Resolver`, `cap.PendingNet`).
  - `netdev/`: `NIC` interface and `ErrDown` (decouples NIC drivers from `net/netstack`).
  - `input/`: input events (`KeyDown/Up`, `Motion`, `Button`, `Touch`, `Wheel`), non-blocking `Broadcaster`, HID `Keyboard` decoder, keymaps (`layout.go`: `us`, `uk`, `dvorak`, `colemak`).
  - `klog/`: 64 KB timestamped ring buffer mirrored to console (`dmesg [-f]`).
  - `rtkit/`: Apple RTKit coprocessor protocol engine over `Mailbox` (tested against fake; wire encodings unverified on hardware).
- `drivers/`: **portable drivers taking `mmio.Regs`, host-tested with `mmio.Fake` (no build tags)**.
  - `arm/{pl011,pl031,pl061}`: UART, RTC, GPIO power button.
  - `virtio/{mmio,net,rng,p9}`: modern virtio-mmio v2 transport + split virtqueues (`AddChain`, `virtiotest`), NIC, RNG (seeds board DRBG), 9P host share.
  - `pci/`, `nvme/`: ECAM host bridge (scans bus 0, assigns BARs, registers `pciclass,CCSSPP`), polled NVMe (read & write).
  - `usb/`, `usb/xhci/`: polled xHCI host, HID boot keyboard/mouse, hubs (`hub.go`, no TT yet), BOT/SCSI mass storage (`storage.go`, read & write, REQUEST SENSE / stall recovery, publishes `usbdisk<N>`), CDC-ECM Ethernet (`ecm.go`, publishes `usbnet<N>`).
  - `simplefb/`: firmware framebuffer text console, 2000-line scrollback, gpm-style pointer/scroll/select/paste (`Mouse`), allocation-free `Early` boot painter.
  - `qemu/fwcfg/`: QEMU `fw_cfg` DMA interface + `SetupRAMFB`.
  - `apple/{wdt,pmgr,gpio,spi,spihid}`: M1 watchdog (`dev.Rebooter`), power domains, GPIO/pinctrl, SPI, polled internal keyboard + multitouch trackpad.
- `fs/`: `memfs` (writable RAM FS, 512 MB cap), `fat` (FAT12/16/32 read + FAT16/32 write with LFN & FSInfo), `ext4` (ext2/3/4 read-only), `ninep` (9P2000.L client, read & write), `rofs` (shared read-only helper), `testdata/` (real `mkfs` images).
- `net/`: `netstack` (gVisor `pkg/tcpip` + `gonet`, DHCP with renewal, DNS — does not use global `net.SocketFunc`), `hostnet` (hosted `cap.Net` on `127.0.0.1`).
- `programs/`: `initd` (PID 1), `sh` (shell + builtins including `ps`, `kill`, `lsdev`, `lsblk`, `df`, `ls`, `cat`, `cp`, `mv`, `rm`, `mkdir`, `write`, `mount9p`, `umount`, `ifconfig`, `host`, `get` HTTP/HTTPS with embedded Mozilla roots, `dmesg`, `pprof`, `evtest`, `kbd`, `reboot`, `halt`, plus leading `KEY=VAL` env vars for `.wasm`), `sshd` (`x/crypto/ssh` + custom SFTP v3 server + `FuzzPreAuth`), `wasm` (wazero interpreter WASI preview 1 runner with SHA-256 LRU compiled-module `Cache` and writable `vfs.Writable` mounts), `coreutils`, `hello`.
- `board/`: platform code behind build tags (`virt`, `applem1`, `microvm`, `hosted`).
- `tools/`: `mkimage`, `mkdisk`, `qemucheck`, `hot`, `fakem1n1`, `m1n1boot`, `ninepd` (host TCP 9P2000.L server), `godeep-partition.sh`, `progress-log.sh`, `qemu-stop.sh`, `mktestfs.sh`, `setup-windows.ps1`, `githooks/pre-commit`.
- `third_party/tamago/`: vendored TamaGo v1.27.4 (`replace` in `go.mod`; see `GODEEP_PATCHES.md`).
- `docs/`: design & hardware docs (see Documentation Index below).

---

## Rules & Conventions

1. **Context first, cancel promptly**: Every blocking kernel/driver API takes `context.Context` first and must return promptly on `ctx.Done()`. Kill is cooperative — a goroutine that ignores `ctx` cannot be killed.
2. **Never use bare `go` in kernel or program code**: Spawn goroutines with `p.Go(fn)` (or `proc.Kthread` for kernel threads) so they are attributed to a PID and panics kill only that Proc.
   - *Accepted exception*: trusted third-party libraries (`gVisor`, `x/crypto/ssh`) spawn internal goroutines (they inherit pprof labels, but panics crash the kernel). Always invoke them from a Proc and fuzz untrusted inputs (e.g., `FuzzPreAuth`).
3. **Capabilities, no global mutable state**: Pass capabilities (`cap.Net`, `*vfs.NS`, `*klog.Log`, etc.) into Procs; do not add global registries or mutable namespaces (exception: read-only `prog.Register` and `dev.Register` during `init()`).
4. **Ownership & channels over shared mutexes**: One goroutine owns each hardware device and mutable resource; transfer ownership (e.g., `*dma.Buffer`) over channels rather than sharing memory under locks.
5. **Strict platform separation**: Platform-specific code lives exclusively in `board/*` behind `//go:build tamago` / `!tamago`. `kernel/*` must never import `board/*`. Core packages and `drivers/*` must compile and pass tests on the host.
6. **Memory safety & `unsafe`**: No `unsafe` outside `board/*`, `kernel/dev`, `kernel/mmio`, and `kernel/dma` atomic word accessors.
7. **Driver contract**:
   - Place portable drivers in `drivers/<vendor>/<device>` taking `func(base uint64) mmio.Regs`.
   - Register via `dev.Register` in board `init()`; implement `Run(ctx, *dev.Device)`; call `d.Ready(svc)` when operational.
   - Service interrupts via `<-line.C` followed by `line.Done()` (oneshot).
   - Use single 32-bit/64-bit atomic MMIO accesses (`mmio.Window` / `atomic.LoadUint32` / `StoreUint32`); never read-modify-write write-1-to-set/clear (W1S/W1C) registers.
   - Put FDT resolution helpers in the driver package so they are host-tested, and pass results (or resolution errors) through `dev.Node.Config`.
8. **Testing**:
   - Run `make test` (`go vet -stdmethods=false ./...` is required because wazero's experimental `sys.File` defines `Seek(int64, int) (int64, Errno)`).
   - Stress-test concurrency, kernel, and driver changes with `go test -race -count=50 ./<pkg>`.
   - Verify bare-metal regressions with `make check-virt` (and `make check-applem1` when touching shared arm64/boot/framebuffer/apple code).
9. **Append-only progress log**: `docs/TargetMyMacProgress.log` is strictly append-only. Add entries only via `tools/progress-log.sh`; never edit, rename, or delete it.

---

## Platform Constraints & Known Gotchas

### arm64 / `board/virt`
- **Idle must sleep, not spin**: `board/virt/idle.go` arms `CNTV_TVAL` and executes `WFI` (requires `gic-version=3`). Note: `goos.Idle(0)` means *"no timer pending"*, not *"due now"*.
- **QEMU 11.1.x HVF WFI bug**: After any device (SPI) interrupt under Apple HVF (`MIDR` implementer `0x61`), `WFI` returns immediately and burns 100% CPU (TCG is unaffected). Under HVF, `board/virt` polls UART (`UARTMode`) and `virtio-net` (2 ms busy / 20 ms idle) to stay at ~1.5–2% host CPU.
- **TamaGo `GIC.DisableInterrupt` bug**: Upstream read-modify-writes `GICD_ICENABLER` (W1C), disabling the whole 32-IRQ bank. `board/virt` writes the single bit directly.
- **Modern virtio-mmio only**: Requires `-global virtio-mmio.force-legacy=false` (version 2 transport).
- **Memory layout**: Go heap = first 3.5 GB; DMA region = top 512 MB of `-m 4G` at `0x1_2000_0000` (first 2 MB hold the copied boot DTB).
- **Entry assembly**: `entry_arm64.s` must stay position-independent until self-relocation completes (keep absolute addresses in `DATA` words; no Go code before early identity map). `IMAGE_BASE` in `entry_arm64.s` must equal `TEXT_START - 0x1000` in the `Makefile`.
- **Single core**: TamaGo lacks arm64 SMP bring-up (`-smp 1`, `GOMAXPROCS=1`).
- **Entropy**: Boots with a counter-seeded DRBG (predictable) until `drivers/virtio/rng` mixes 64 bytes of host entropy (`lsdev rng0`).

### arm64 / `board/applem1`
- **1 TB DRAM base**: M1 DRAM starts at `0x100_0000_0000` (1 TB). Relies on vendored TamaGo patches (`HeapAddrBits=41` in `goos/mem.go` and `linkhwinit0` in `arm64/init.go`) plus `kernel/pgtable` (4-level, 48-bit VA).
- **EL2 VHE drop**: Apple cores force `HCR_EL2.E2H=1`; `entry_arm64.s` checks `E2H` before configuring `CPTR_EL2`/`CNTHCTL_EL2` and drops to EL1.
- **No AIC2 / no entropy yet**: Idle uses `WFE` woken by the generic-timer event stream (`CNTKCTL_EL1.EVNTEN`); SPI keyboard/trackpad are polled (ready GPIO every 5 ms, blind-read fallback after 3 s). RNG still seeds from the boot counter.
- **Watchdog & early fault screen**: 30 s watchdog is armed and petted every 10 s; `goos.Exit` and `lateFault` stop it so halt/fault screens stay readable. Early boot draws numbered stage squares (1–4) and a red fault screen (`ESR`/`ELR`/`FAR`) via `//go:nosplit` `simplefb.Early` (`godeep.testfault=N` triggers test faults).
- **DMA alignment**: Apple DART requires 16 KB-aligned buffers (`dma.Alloc(size, 16384)`).

### amd64 / `board/microvm`
- **Needs `-m 3G` or more**: RAM is mapped up to `0xb0000000`; less RAM causes a silent triple fault on boot.
- **No `linkhwinit0`**: Do not pass `linkhwinit0` on amd64.
- **No KVM on macOS**: Runs under TCG (`-cpu max`); no ACPI power-button handler yet (`make stop` falls back to SIGTERM).

### TamaGo General
- No `cgo`, no `plugin`, no `os/exec`, and no CPU profiling timer (`pprof` supports `goroutine`, `heap`, `allocs`, `block`, `mutex`, `threadcreate`).
- Image size is ~12 MB with gVisor, `x/crypto/ssh`, `wazero`, `net/http`, and embedded TLS roots.

---

## Documentation Index

- `README.md`: Getting-started tutorial and shell walkthrough.
- `docs/README.md`: Full documentation index with status tags (current, active roadmap, historical).
- `docs/architecture.md`: Current phase status and original Phase-0 design proposal.
- `docs/DriverModel.md`: Driver rules, DMA, IRQ bridge, device manager, RTKit, and hardware bugs found.
- `docs/TargetMyMac.md`: M1 Pro hardware inventory, Go driver coverage, native boot status, and milestones.
- `docs/PartitioningMac.md`: macOS partitioning runbook, ESP install, QEMU rehearsal, and boot/fault diagnostics.
- `docs/Using2Computers.md`: Tethered two-machine workflow (Windows/WSL2 dev host + Mac target via m1n1 proxy/hypervisor).
- `docs/SSHServer.md`: `sshd`, networking, SFTP, WASM loading, and serial-SSH bridge design.
- `docs/ReUsingGVisor.md`: Analysis of gVisor reuse (netstack vs. Sentry) and capability-oriented syscall equivalents.
- `docs/GoGpuSupport.md`: Display and GPU/compute roadmap (`ramfb`, `virtio-gpu`, SIMD, host offload).
- `third_party/tamago/GODEEP_PATCHES.md`: Rationale for the 3 vendored TamaGo patches.
