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

Isolation comes from the language: everything runs in one address space, and Go's memory safety stands in for the MMU. The kernel runs bare-metal on **TamaGo** (`GOOS=tamago`), with three board targets:
- **arm64 on QEMU `virt` with HVF acceleration** (the default; native speed on Apple Silicon)
- **arm64 on Apple M1 Pro (`board/applem1`)**, booted via m1n1 and rehearsed on QEMU `sbsa-ref` (`tools/fakem1n1`)
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
make check-virt # headless QEMU virt: PCIe + NVMe + GPT + FAT32/ext4, USB kbd/mouse/hub/storage, ramfb, rng, net, SSH/SFTP, WASM, 9P, pprof, usbnet (tools/qemucheck)
make check-usbnet # headless QEMU virt test using USB CDC-ECM (usb-net) as sole NIC
make check-pcie # headless QEMU virt: NVMe + SD card (sdhci-pci) behind pcie-root-ports, and the write fence on a Mac-layout disk (serial godeep-mac)
make check-smp SMP=4 # headless QEMU virt with 4 cores: all started, two CPU-bound pipelines use >1.5 host cores, shell answers under load (check-virt runs it)
make check-uas  # headless QEMU virt: a usb-uas disk on a USB 2 port (UAS without streams): lsblk, ext4/FAT reads, a FAT write read back
make check-iommu # headless QEMU virt with an SMMUv3 in front of PCIe: NVMe, xHCI, USB stick, USB Ethernet DMA through bus addresses
make qemu      # boot it with an interactive shell on serial (arm64: the Image + device tree; BOOT=elf for the ELF; EL2=1 to enter at EL2 like m1n1, TCG)
make qemu-applem1   # the Mac image itself on QEMU sbsa-ref, handed over like m1n1 (tools/fakem1n1); TCG
make shot-applem1   # (another terminal) its framebuffer → bin/fakem1n1/screen.png
make check-applem1  # headless: Mac image reaches the shell, and the fault screen works (stages 1, 3)
make native    # this Mac: host tests, build the Mac image, check-applem1, then install it on the godeep ESP (asks to type "godeep" and for sudo; DRY=1 to preview)
make stop      # (another terminal) power button → clean shutdown; falls back to SIGTERM/SIGKILL
make kill      # (another terminal) SIGKILL QEMU
make hot       # make qemu that rebuilds and reboots on every .go/.s change (tools/hot): terminal stays attached (serial over a socket), a failed build keeps the old VM, clean QMP power-off between images; Ctrl-A x quits, Ctrl-A r reboots now. Stop make qemu first (same port, pidfile, QMP socket)
make test runs `go vet -stdmethods=false`: wazero's sys.File needs a `Seek` that returns an Errno.
make wasm      # build examples/wasm/* for WASI → bin/wasm/NAME.wasm
make push      # scp bin/wasm/$(WASM).wasm (default hello) into godeep's ram:; run it in the shell as ram:/hello.wasm ARGS
make keys      # dev SSH keys in bin/ssh, copied into programs/sshd/keys/ (gitignored) and built into the image; kernel/hosted depend on it
make ssh       # ssh into godeep on localhost:$(SSH_PORT) (2222: make qemu's hostfwd, or make hosted); make ssh CMD='lsdev net0'
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
  - `tty`: line discipline, run as its own kernel goroutine (`New`) or scoped to a Proc (`NewFor` for SSH): echo, cursor editing, 100-line history, typeahead, ^C → `Interrupts()` channel
  - `prog`: registry of built-in programs (registered from `init()`, read-only after boot)
  - `dma`: DMA allocator over a region outside the Go heap + `IOMMU` interface (`Identity`; Apple DART to do). `Buffer.Load32/Store32`: acquire/release access to words a device shares (completion phase bits, ownership flags)
  - `block`: `block.Device` and `block.Writer` interfaces, GPT/MBR partitions, cached `io.ReaderAt`/`io.WriterAt` (`NewReader`); `MakeGPT` for tools/tests
  - `irq`: interrupt bridge. The board acks and calls `Dispatch`; the line is masked until the driver calls `line.Done()` (oneshot)
  - `dev`: device manager. Each driver is a process `drv/<name>` under `devmgr`, with dependencies (`Needs`), readiness (`Ready`/`Get`) and restart-on-crash
  - `rtkit`: Apple RTKit coprocessor protocol over a `Mailbox` interface (encodings unverified on hardware)
  - `fdt`: device-tree parser (`ranges` translation, GIC interrupt cells, phandles: `Refs` for `*-gpios`/`power-domains`/`interrupts-extended`); boards build `dev.Node`s from the tree in `x0`. `dev.Node.Config` carries board-resolved driver settings (or an error the driver returns, so lsdev shows it)
  - `mmio`: `Regs` interface for device registers: `Window` (tamago only) and a recording `Fake` for host tests
- Networking (`docs/SSHServer.md`): `drivers/virtio/mmio` is the modern (version 2) virtio-mmio transport (`Init`: reset + feature negotiation; `Queue`: split virtqueue, one buffer per descriptor, `Take`/`Push`/`Kick`/`Used`) plus `virtiotest`, a device model for host tests. `drivers/virtio/net` (virtio-net, publishes a `netdev.NIC`: whole Ethernet frames; irq, or polled under HVF: 2 ms while busy, 20 ms idle) and `drivers/virtio/rng` (seeds the board's DRBG through `Options.Seed`). Boards rename virtio slots with a driver to `virtio,device<ID>` and `net0`/`rng0`. `net/netstack`: gVisor TCP/IP (IPv4, ARP, TCP, UDP, ICMP) on a NIC via a channel endpoint; `RunRX`/`RunTX` pumps; `Listen`/`Dial` through gVisor's `gonet` (tamago's global `net.SocketFunc` is not used). `kernel/net.go`: `netd` finds the first NIC (10 s), runs the stack with DHCP (+ lease renewal, falling back to QEMU's user-net addresses 10.0.2.15, gw 10.0.2.2; `Board.NetConfig`), follows the NIC across driver restarts, and resolves a `cap.PendingNet`. `net/hostnet`: hosted `cap.Net` on 127.0.0.1, ports < 1024 + 2200. `programs/sshd`: `x/crypto/ssh`, public-key auth against keys built into the image; each session = `tty.NewFor` over the channel + a child `sh` (exec requests run `sh -c LINE`); kill-on-disconnect; started by `initd` when the board has a network. Shell: `ifconfig`
- Files: `kernel/vfs` is the namespace a Proc is given (`sh.Env.FS`, not a global): mounted volumes (`ram`) plus every disk/partition the device manager has, FAT/ext4 recognized on open (`OpenFS`, moved from the shell). Paths are `VOLUME:PATH` in the shell, `/VOLUME/PATH` over SFTP. `vfs.Writable` volumes accept writes (`fs/memfs` for `ram:`, 512 MB cap, lost at reboot; writable `fs/fat` on writable disks; writable `fs/ninep`). Shell: `df`, `ls`, `cat`, `cp`, `mv`, `rm`, `mkdir`, `write`
- Host share: `drivers/virtio/p9` (virtio-9p; tag from the config with byte reads, `mmio.ByteRegs`/`Config8`, since QEMU answers all-ones to 32-bit reads past the end) + `fs/ninep` (9P2000.L client as `io/fs.FS`, read and write; host-side `ninep.Serve` for tests/tools) + `tools/ninepd` (host-side TCP 9P2000.L server). The Makefile shares the repo read-only as `host`, and shell builtins `mount9p HOST:PORT NAME` / `umount NAME` mount TCP 9P shares over `cap.Net` (`tools/qemucheck -ninep` exercises this in `check-virt`). Devices can publish volumes by name (`vfs.Named`). Virtqueues take descriptor chains (`Queue.AddChain`)
- `programs/sshd/sftp.go`: an SFTP v3 server (pkg/sftp doesn't build for tamago; its client tests ours on the host). `/` lists volumes, relative paths start at `/ram`, so `scp f godeep:` lands in RAM; only writable volumes accept writes
- `programs/wasm`: WASI preview 1 programs on wazero's interpreter (the compiler needs executable memory), run as ordinary Procs (ps, kill, ^C), with a capability-owned SHA-256 LRU compiled-module cache (`wasm.Cache` in `sh.Env`) so repeat runs skip decode/compile. The shell runs any `[KEY=VAL...] VOLUME:PATH.wasm ARGS`; stdout to the terminal, stdin line by line from it, every volume at `/NAME` with a synthetic `/` listing them: writable volumes (`ram`, writable FAT/9P) read-write through wazero's experimental `sys.FS` (`programs/wasm/sysfs.go`), the rest read-only. Examples in `examples/wasm/` (plain Go, `GOOS=wasip1 GOARCH=wasm`). No reboot to try new code: `make push`, or run straight from `host:` / `mount9p`
- `kernel/klog`: the kernel log (64 KB ring, lines stamped with time since boot, mirrored to the console, followable). `sh.Env.Log`; netd, sshd and shutdown log to it; `dmesg [-f]`
- `pprof NAME [1|2]` (shell builtin): runtime profiles (goroutine, heap, allocs, block, mutex, threadcreate) as protobuf, so `ssh godeep pprof heap > heap.pb.gz && go tool pprof heap.pb.gz`; 1/2 print text. No CPU profile: TamaGo has no profiling timer
- DHCP and DNS (`net/netstack/dhcp.go`, `dns.go`): netd gets a lease (8 s, else the static QEMU addresses) and renews at half-lease time; `Dial` takes names, `cap.Resolver`. Shell: `host NAME`, `get http[s]://...` (TLS verified against embedded Mozilla root certs via `golang.org/x/crypto/x509roots/fallback`)
- `kernel/cap`: capability interfaces. `cap.Net` (Listen/Dial), `PendingNet` (handed out at boot, resolved when the stack is up). `kernel/netdev`: the `NIC` interface and `ErrDown`, so drivers don't import the stack
- `drivers/pci`: ECAM host bridge (`pci-host-ecam-generic`): scans bus 0, assigns BARs from `ranges` (no firmware does it), adds each function as a device named by class (`nvme0`) with compatible `pciclass,CCSSPP` and Config `*pci.Function` (`dev.Device.Add`: drivers can add devices they discover)
- `drivers/nvme`: NVMe over PCIe, polled, read and write; one goroutine owns the controller, requests go over a channel; publishes a `block.Device` (and `block.Writer`)
- `fs/fat`, `fs/ext4`: filesystems as `io/fs.FS` (FAT12/16/32 read + FAT16/32 write with LFN & FSInfo; ext2/3/4 read-only: extents, block maps, 64-bit, htree, symlinks), over the shared `fs/rofs`. Tested against real `mkfs` images in `fs/testdata` (`tools/mktestfs.sh`, needs `brew install e2fsprogs dosfstools mtools`) and `testing/fstest`. Shell: `lsblk`, `ls DISK:PATH`, `cat DISK:PATH` (no mount table: `nvme0p2:/docs`)
- `kernel/input`: input events (`KeyDown/Up`, `Motion`, `Button`, `Touch`, `Wheel`), `Broadcaster` (non-blocking fan-out; a driver's service implements `input.Source`, or `input.Sources` for several), the shared HID keyboard decoder (`Keyboard`: modifiers + 6 usages → console bytes, key events, repeat), and keyboard layouts (`layout.go`: `us`, `uk`, `dvorak`, `colemak`). Shell: `evtest [SECONDS]`, `kbd [LAYOUT]`
- `drivers/usb` (descriptors, requests, HID boot keyboard/mouse) and `drivers/usb/xhci`: polled xHCI host. Hubs (`hub.go`: status-change endpoint, port power/reset, route strings; no transaction translator yet, so low/full-speed devices behind a high-speed hub won't work), mass storage (`storage.go`: Bulk-Only Transport + SCSI INQUIRY/TEST UNIT READY/READ CAPACITY/READ(10)/WRITE(10)/REQUEST SENSE + stall recovery; each disk is added as `usbdisk<N>`, compatible `godeep,usb-storage`, a `block.Device`/`block.Writer`, so lsblk/vfs/FAT/ext4 just work; I/O runs on the controller's goroutine), and CDC-ECM Ethernet (`ecm.go`: publishes `usbnet<N>`, compatible `godeep,usb-ecm`, a `netdev.NIC`; tested via `make check-usbnet`). HID boot keyboards type into the console (`board/virt`'s `consoleIn`), mice publish events. QEMU: `qemu-xhci`, `usb-kbd`, `usb-mouse`; input injected over QMP in `check-virt`
- `tools/mkdisk`: `bin/disk.img` (GPT: FAT32 + ext4 test images), QEMU's NVMe disk. `tools/qemucheck`: boot QEMU, type commands, expect output; `send:`/`wait:` steps, `keys:`/`qmp:` steps through QEMU's QMP socket (USB keyboard and mouse), `local:CMD=>WANT` runs a command on this machine (ssh, scp) with retries, a `CMD=>WANT` step is retried until its output matches, and `-http ADDR=DIR` serves files for the guest's `get`
- `drivers/<vendor>/<device>`: portable drivers taking `mmio.Regs`, host-tested; boards register them (`drivers/arm/pl061`, `pl031`, `drivers/virtio/mmio`, `drivers/apple/wdt`: the Apple watchdog, a `dev.Rebooter` for the shell's `reboot`; `drivers/apple/{pmgr,gpio,spi,spihid}`: the internal keyboard, polled like U-Boot's, see below)
- `tools/mkimage`: ELF → arm64 Linux Image (`-text-offset` override to test self-relocation)
- `tools/m1n1boot`: puts godeep in place of U-Boot in an Asahi `m1n1/boot.bin` (m1n1 boots godeep directly; U-Boot on this Mac autoboots Omarchy). `-info` tells which payload a boot.bin has
- `tools/fakem1n1`: m1n1 stand-in for QEMU `sbsa-ref`, whose RAM starts at 1 TB like the M1's, so `bin/godeep-applem1.img` runs unmodified. An EL3 shim (`shim.asm`, words in `shim_code.go` via `go generate`) drops to EL2 with E2H=1. It loads the Image away from its link address and passes an m1n1-style tree (`/chosen` framebuffer, `/memory`, `/reserved-memory`, Apple-style nodes, plus a PL011 so the console is mirrored to the terminal). `shot` = framebuffer PNG over QMP; `check -fault N` = fault-screen test. sbsa-ref puts its own DTB in the first MB of RAM, so the shim sits at +2 MB
- Early boot screen (`board/applem1/early.go`): numbered squares on m1n1's framebuffer for each boot stage (1 Hwinit0, 2 final page tables, 3 Hwinit1, 4 runtime up), and a red fault screen (ESR/ELR/FAR) from early vectors (`entry_arm64.s` → `early_arm64.s`), then via `arm64.SystemExceptionHandler` until the console exists. Built on `fdt.ScanEarly` and `simplefb.Early`: allocation-free, and the painter is `//go:nosplit` (it runs on a private stack). `godeep.testfault=N` in `/chosen/bootargs` faults at stage N on purpose
- `kernel/pgtable`: 4-level 48-bit page tables, allocation-free (runs in `Hwinit0`, before the Go runtime)
- `drivers/simplefb`: text console on a firmware framebuffer. It keeps the text (screen + `History` 2000 lines), so `Mouse` (gpm-style, fed every input event) can: move a pointer (drawn over the text, pixels saved under it; shown after the first motion), scroll history with the wheel / two-finger scroll (any key or output returns to the live screen), left-drag to select (inverted), right/middle click to paste into the console input (`PasteTo`). `lsdev fbcon` shows its state (`simplefb.Driver`, Config = the console)
- `kernel/inputd.go`: when the board sets `Console.Input`, `inputd` subscribes to every input source in the device manager (rescans: 100 ms while devices appear, then 1 s; drops sources whose driver restarted) and hands events to it one at a time. Both boards set it to a `simplefb.Mouse`
- `drivers/qemu/fwcfg`: QEMU fw_cfg over its DMA interface only (the selector register is 16-bit-only); `SetupRAMFB` gives `board/virt` a 1280x800 framebuffer console when QEMU has `-device ramfb` (in `QEMU_NET`). Console output then goes to ramfb and the UART; `make qemu QEMU_DISPLAY=cocoa` shows it in a window
- `board/applem1` (`tamago && arm64 && applem1`, `make image BOARD=applem1`): this MacBook Pro. **All cores** (`smp.go`, `smp_arm64.s`): m1n1 parks the other cores on a spin table (`/cpus/*` `enable-method = "spin-table"`, `cpu-release-addr`); the board writes `secondaryEntry` there (non-cacheable mapping, `DC CIVAC`) and sends SEV; each core drops EL2 (VHE) → EL1 like the boot core, loads its MMU/vectors/event stream, finds its slot by MPIDR, and waits for `goos.Task`. `goos.Wake` = per-core flag + SEV (idle is WFE). fakem1n1 rehearses it (`-smp N`: every core starts in the shim, which parks cores ≠ 0 on its spin table indexed Aff1×8+Aff0; `check-applem1` runs 10 cores); `lsdev idle` shows "N of M cores". Not yet booted on the Mac. `godeep.nosmp` in `/chosen/bootargs` keeps it on the boot core: `make native BOOTARGS=godeep.nosmp` (`tools/m1n1boot -bootargs` adds a `chosen.bootargs=` line before the kernel payload). Boots to the shell under QEMU sbsa-ref via `make check-applem1`, and on the real Mac via m1n1 (2026-11-25); the internal keyboard and trackpad work there since 2026-11-26 (`screens/IMG_2342.JPG`). Idle = WFE woken by the generic-timer event stream (`idle.go`; no interrupts needed; under QEMU TCG WFE doesn't sleep). The watchdog (30 s) is stopped by `goos.Exit` and `lateFault` so halt/fault screens stay. fakem1n1's watchdog node is a RAM page: it is wired but never counts or resets. Keyboard and trackpad: `spihid.ConfigFromFDT` resolves the transport node (power-domain chains, spi3 pinmux, spien, ready line) and the board feeds its bytes into the same console channel as autorun. It polls the ready line every 5 ms, or falls back to blind reads after 3 s; lsdev's `hid-transport@0` row reports mode, power, packets, CRC and keys. The trackpad (device 2) is switched to multitouch with one set-report write (`Config.Trackpad`) after the device's booted packet; one finger moves the pointer, two scroll, click is button 0 (`evtest`). Messages longer than a packet are reassembled. fakem1n1 backs this path with preloaded RAM pages (`keyboard.go`, `regs.bin`): it exercises bring-up and the fallback, not typing. On QEMU you type through the PL011 instead (`drivers/arm/pl011`, polled, same input channel); `check-applem1` types `help` and expects the shell's answer. See `docs/TargetMyMac.md` §3.1 and `docs/PartitioningMac.md`.
- `third_party/tamago`: vendored TamaGo, via `replace` in go.mod: arm64 `linkhwinit0` (2 files), and `goos.HeapAddrBits` 41 instead of 40 (M1 DRAM starts at 1 TB = 1<<40; with 40 the runtime aborts in `schedinit`). See its GODEEP_PATCHES.md
- `docs/TargetMyMacProgress.log` (then `.2.log`, `.3.log`… when a part nears 48 MiB): append-only, timestamped progress log of the native Mac work, kept in git. Add entries only with `tools/progress-log.sh`; never edit, rename or delete a part (append a correction instead). The pre-commit hook (`tools/githooks/pre-commit`, enabled with `git config core.hooksPath tools/githooks`) refuses commits that do; `tools/progress-log.sh --check` runs the same check
- `tools/godeep-partition.sh`: guided partitioning of this Mac (`docs/PartitioningMac.md`)
- `tools/setup-windows.ps1`: Windows 11 setup script for the two-computer workflow (`docs/Using2Computers.md`)
  - See `docs/DriverModel.md` for the driver rules and these four pieces
  - `ipc`: byte pipes (`Pipe`: buffered, `CloseWithError`; a Proc's `Spec.Ends` are closed when it dies, which wakes the other side) and typed request/reply ports (`Port[Req,Resp]`: `Serve`/`Call`) in a `Namespace` handed out as a capability (`sh.Env.Ports`; `Only` narrows it)
  - `gpu`: the display capability (`Display`, `Surface`: XRGB8888 pixels + `Present(damage)`); a surface dies with the ctx it was created with
- `docs/Using2Computers.md`: the Windows PC (WSL2) as dev host, the Mac as a tethered target through m1n1's proxy and hypervisor
- `docs/SSHServer.md`: plan for `programs/sshd` (`x/crypto/ssh`): sessions = `tty.New` over the channel + `sh`; over virtio-net/gVisor on QEMU, or over a UART via `ProxyCommand` under m1n1's hypervisor on the Mac
- `board/*`: the only places with platform code, behind build tags
  - `board/virt` (`tamago && arm64`; entry code needs the `linkcpuinit` tag, set in the Makefile): **written by us**. `entry_arm64.s` replaces TamaGo's `cpuinit`: EL2 → EL1, and it copies the boot device tree to the start of the DMA region (2 MB reserved) before the Go heap exists. Devices come from that tree (`devices.go`), or a static list for ELF boots. We wrote this board because TamaGo has no arm64 QEMU-virt board. It provides the TamaGo runtime hooks via `//go:linkname`: `RamStart`/`RamSize`/`RamStackOffset`, `Hwinit1`, `Nanotime`, `Printk`, `InitRNG`/`GetRandomData`. It also has a PL011 UART driver, a GICv3 + virtual-timer idle governor, and PSCI power-off.
  - `board/microvm` (`tamago && amd64`): wraps TamaGo's `board/qemu/microvm`
  - `board/hosted` (`!tamago`)
  - `cmd/godeep/board_<arch>.go` picks the board for each GOARCH
- `programs/…`: `initd` (PID 1, respawns sh), `sh` (shell + builtins `ps`, `kill`, `mem`…), `coreutils` (demo programs and text filters `grep`, `wc`, `head`, `tail`, `sort`, `yes`, which read `p.Stdin`)
- Shell command lines (`programs/sh/pipeline.go`): quotes, `a | b`, `< VOL:PATH`, `>`/`>> VOL:PATH`, `&`. Each stage is a Proc joined by `ipc` pipes; a builtin alone in the foreground runs inside the shell, otherwise as its own Proc (builtins print to `env.out()`, read `env.input()`). Programs get stdin from the terminal a line at a time (`ttyReader`). `restrict [-vol A,B] [-ro] [-net none|dial] [-ports A,B] [-nodev] CMD` runs CMD with narrowed capabilities (`vfs.NS.View`, `cap.Restricted`, `ipc.Namespace.Only`); SSH sessions get a dial-only `cap.Net`. `top` samples goroutine states per Proc (and `Proc.IO` byte counts); `kvd`/`kv`/`ports` demo ipc ports
- `fs/synth`: generated read-only filesystems; `fs/procfs` builds `proc:` (`/PID/status|cmdline|goroutines`, `meminfo`, `uptime`, `version`) and `sys:` (`/DEVICE/state|compatible|pid|irq|reg|error|info`), mounted by the kernel
- `drivers/virtio/gpu`: virtio-gpu 2D (`-device virtio-gpu-device,id=gpu0`, in `QEMU_NET`): one scanout, polled control queue, publishes a `gpu.Display` as `gpu0`. Shell `gpu [test [SECONDS] | fill RRGGBB | off]`; check-virt screendumps it over QMP and checks the bars with `tools/pngpix`
- USB HID in report protocol (`drivers/usb/hid.go`: report-descriptor parser, report IDs, buttons, relative/absolute X/Y, wheel, keyboards; fuzzed): non-boot HID interfaces (QEMU `usb-tablet`, port 4) → `input.Absolute` events (0..`input.AbsMax`), which `simplefb.Mouse` maps to the screen. UAS (`drivers/usb/xhci/uas.go`): USB Attached SCSI without streams (USB 2 links; on SuperSpeed the device's BOT alt setting), same SCSI layer as BOT (`Disk.xfer`), autosense from the Sense IU

## Rules

- Every kernel API takes `context.Context` first and **must return promptly when it is cancelled**. This is what makes kill work.
- Never use a bare `go` statement in kernel or program code. Start goroutines with `p.Go(fn)` so they are accounted to a Proc and their panics are contained (a panic kills the proc, not the kernel). Exception we accept: libraries (gVisor, `x/crypto/ssh`) start their own goroutines. They inherit the caller's pprof labels (so `ps` attributes them), but their panics stop the kernel: call them from a Proc, and fuzz untrusted input (`programs/sshd` `FuzzPreAuth`).
- There are no global mutable namespaces. Authority comes only from capabilities passed to a Proc.
- Prefer channels and ownership (one goroutine owns the state) over shared memory with mutexes.
- Platform-specific code lives only in `board/*`, behind `//go:build tamago` / `!tamago`.
- New core code needs host unit tests, run repeatedly with the race detector (`go test -race -count=50`). Repetition found most of the bugs so far.
- Drivers: one goroutine owns the device; register with `dev.Register` from `init()`; implement `Run(ctx, d)`; call `d.Ready(svc)`; service interrupts with `<-line.C` then `line.Done()`.
- Device registers: single 32-bit `atomic.Load/StoreUint32` accesses (HVF can't decode paired MMIO). Never read-modify-write a write-1-to-set/clear register.

## Constraints to remember

arm64 / `board/virt`:
- **Idle must sleep, not spin.** TamaGo's default arm64 idle only `WFI`s when no timer is pending, so with any sleeping goroutine it burns 100% of a host core. `board/virt/idle.go` arms the virtual timer (`CNTV_TVAL`) for the next deadline and runs `WFI` with interrupts masked in PSTATE. Under HVF, QEMU sleeps the vCPU until `CNTV_CVAL`; under TCG the timer PPI (INTID 27) wakes the core through the GIC. This needs `gic-version=3`. Measured idle cost: about 1.5% of a host core.
- **The RNG starts seeded from the boot-time counter: not secure entropy.** With `-device virtio-rng-device` (the Makefile adds it), `drivers/virtio/rng` mixes 64 bytes of host entropy into the DRBG as soon as it starts (`lsdev rng0`: "seeded crypto/rand"); randomness used before that is predictable. `board/applem1` has no entropy source yet.
- **virtio-mmio must be modern:** QEMU's virt machine defaults to the legacy (version 1) transport; godeep only speaks version 2, so QEMU needs `-global virtio-mmio.force-legacy=false` (in `QEMU_NET`).
- **Network on QEMU:** `-netdev user` with `hostfwd=tcp:127.0.0.1:$(SSH_PORT)-:22` (2222; check-virt uses 2223). The NIC is polled under HVF (WFI bug, below); idle stays ~2% of a host core. The image is ~12 MB with gVisor, SSH, wazero and net/http (was 2.4 MB).
- **Device access under HVF must be single 32/64-bit loads/stores** (e.g. `atomic.Load/StoreUint32`). Paired or vector accesses to MMIO can't be decoded by the hypervisor.
- **SMP** (`make qemu SMP=4`; default 1; `make check-smp`): `board/virt/smp.go` starts every core in the device tree with PSCI `CPU_ON` (EL1 boots only) at `secondaryEntry` (`smp_arm64.s`: the boot core's TTBR0/TCR/MAIR/SCTLR/VBAR, FP on, GIC CPU interface, then WFE until `goos.Task` hands it an M), wakes each core's GIC redistributor, sets `goos.ProcID`/`Task`/`Wake` (a Group 0 SGI) and `GOMAXPROCS` = cores. Interrupts: device SPIs route to the boot core, whose IRQs stay unmasked; `irqTop` (`irq.go`, run *inside* the exception through the vendored TamaGo `arm64.IRQHandler` hook, nosplit, no allocation) acknowledges on that core, masks the line at the distributor, EOIs and records it; the `irq` goroutine dispatches from any core (a goroutine can't be pinned on TamaGo: `LockOSThread` is a no-op). Secondaries never unmask IRQs; `idle` acknowledges their SGI/timer (`drainLocal`). `gicController.Enable` writes single bits (no read-modify-write of `ISENABLER`, which could re-enable a line `irqTop` just masked). Idle cost with 4 cores ≈ 1 vCPU (~1.6% of a host core). Per-core idle/wake/SGI/timer counters: `lsdev cpus`. `burn N S` (coreutils) measures the speed-up: 1 core 2.3 GB/s, 4 cores 6.7, 10 cores 15.7
- **TamaGo scheduling limits (matter more with SMP):** (1) no preemption at all (no sysmon, no async preemption): a goroutine that neither blocks nor allocates keeps its core, and the timers queued on that P, forever; on one core that hangs godeep (CPU-bound loops must `runtime.Gosched()`, as `burn` does). (2) Idle Ms never park: TamaGo's `beforeIdle` keeps every M looping with its P, so `wakep` finds no idle P and never calls `goos.Wake`; a core picks up work on another core's queue only when its own idle sleep ends (timer, or ~0.7 ms event stream on the Mac). Fine-grained cross-core hand-offs (pipes of small writes) are therefore much slower with several cores. Both need a tamago-go runtime patch (a preemption tick; parking secondary Ms in `stopm` so `goos.Wake` is used)
- Memory: Go heap = first 3.5 GB (`RamSize`); DMA region = last 512 MB of `-m 4G` at `0x1_2000_0000`, mapped as normal memory in `Init`.
- Don't use TamaGo's `GIC.DisableInterrupt`: it read-modify-writes `GICD_ICENABLER` and disables the whole bank. `board/virt` writes the single bit.
- `goos.Idle(until)`: `until == 0` means "no timer", not "due now".
- Entry code (`entry_arm64.s`) must stay position-independent until relocation: Go assembles `MOVD $sym(SB)` as PC-relative (ADRP+ADD), so keep absolute addresses in DATA words. No Go code before the early identity map: with the MMU off, Go's vector loads/stores fault on Device memory.
- `IMAGE_BASE` in each board's entry_arm64.s must equal the Makefile's `TEXT_START - 0x1000`.
- **QEMU 11.1.x HVF WFI bug:** after the first device (SPI) interrupt, WFI returns immediately and the idle VM spins at 100%. TCG is fine and the guest state is clean. Under HVF (`MIDR` implementer `0x61`) the UART is polled (`UARTMode`) to avoid it. To measure idle cost: `ps -o %cpu= -p $(cat bin/qemu-arm64.pid)`. Debug counters must not add Go timers or keystrokes, since both change idle behaviour.
- **Power button:** QEMU `system_powerdown` (sent by `make stop` over QMP) raises PL061 GPIO line 3 (`0x09030000`). `drivers/arm/pl061` (wired in `board/virt/drivers.go`) handles the interrupt and signals `kernel.Boot`, which kills every process (`Table.Shutdown`, 2 s grace) and calls PSCI off. This works with the default `-machine virt` (no `acpi=off` needed when booting with `-kernel`). amd64 has no power-button handler, so `make stop` falls back to SIGTERM there.

amd64 / `board/microvm`:
- **QEMU needs `-m 3G` or more.** The microvm board maps RAM up to `0xb0000000`. With less memory, the stack lands in unbacked RAM and the guest silently triple-faults (jump to RIP=0).
- macOS has no KVM, so QEMU runs under TCG (`-cpu max`). tamago detects TCG and calibrates the TSC via the LAPIC timer. Don't use `kvmclock`/`-enable-kvm` flags there.
- Don't build with the `linkhwinit0` tag (tamago-example uses it): it removes the default `Hwinit0`, and the link fails unless we provide our own.
- To debug boot crashes, add `-d int,cpu_reset` to QEMU and look for `Triple fault` and the RIP/SP before it.

- **Kill is cooperative.** Go cannot preempt-kill a goroutine. A cancelled Proc dies at its next blocking kernel call.
- TamaGo has no cgo, no `plugin` and no `os/exec`. Dynamic programs run via the `wazero` WASI interpreter (`programs/wasm`).
- Single address space means trust: only memory-safe Go (no `unsafe` outside `board/*`, `kernel/dev`, `kernel/mmio` and `kernel/dma`'s word accessors).
