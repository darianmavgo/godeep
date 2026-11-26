# godeep driver model

How drivers are structured, and the four kernel pieces every driver sits on. These were built before any individual Apple Silicon driver, because nearly all of the roughly 40 devices in `docs/TargetMyMac.md` depend on them.

## The rules

1. **One goroutine owns each piece of hardware.** Nobody else touches its registers; other code sends it requests over channels.
2. **Layered, no cycles.** Requests go down, events come up. Two drivers blocking on each other's channels is the first deadlock you'll hit otherwise.
3. **Channels carry ownership, not bulk data.** Hand a `*dma.Buffer` to another goroutine and you no longer own it.
4. **Every blocking `select` includes `ctx.Done()`.** That's how kill, restart and shutdown work.
5. **A driver is a process.** It shows in `ps`, a panic kills only that driver, and `devmgr` restarts it.

## The four pieces

### 1. `kernel/dma`: device-visible memory

- `dma.New(Region)` manages memory **outside the Go heap**. The GC never moves or frees it, so physical addresses stay valid while a device uses them.
- `Alloc(size, align)` returns a zeroed `*Buffer` with `Phys` (address) and `Bytes` (CPU view):
  - minimum 128-byte alignment (the M1's cache line);
  - use `align = 16384` for anything an Apple DART maps.
- `Free` panics on double free, because two owners of a DMA buffer is a memory-corruption bug.
- `Load32`/`Store32`: acquire/release access to words a device shares. Poll a completion's phase bit with `Load32` (a plain load can be hoisted out of the loop, or the rest of the entry read before the flag); hand an entry over with `Store32` after writing its body. The only `unsafe` outside `board/*` and `kernel/dev`.
- `IOMMU` interface:
  - `Map(buf, dir) → iova`. Devices get the IOVA, never `Phys`.
  - `Identity` is the IOMMU for machines without one (QEMU virt).
  - An Apple **DART** implementation is still to do.

On QEMU virt, the top 512 MB of `-m 4G` (`0x1_2000_0000`) is the DMA region. `RamSize` hands the Go heap only the first 3.5 GB, and the board maps the region as normal memory. See it with `dmastat`.

### 2. `kernel/irq`: the interrupt bridge

```
device asserts line → exception (TamaGo) → "irq" kernel thread acks it at the
controller → Bridge.Dispatch(id): mask the line, non-blocking send on its
channel → driver goroutine wakes on <-line.C, services the device → line.Done()
unmasks it
```

- **Oneshot pattern:** the line stays masked from dispatch until the driver calls `Done`, so level-triggered lines can't re-fire before their driver runs. Several interrupts before the driver runs coalesce into one event (buffer size 1).
- `Controller` is the hardware interface: GICv3 on QEMU virt (`board/virt/board.go`), AIC2 on M1 (to do).
- `irqstat` shows per-line counts and service time. `irqlat` measures timer-deadline → driver-goroutine latency, using the physical timer.

Measured on this Mac:

| Setup | Median latency |
|---|---|
| HVF, CPU awake | about 11 µs end to end |
| HVF, CPU asleep in WFI | about 300 µs, which is the host waking the vCPU thread |
| TCG | about 320 µs |

### 3. `kernel/dev`: the device manager

- A board lists its hardware as `dev.Node`s: name, `compatible`, register windows, IRQs, and `Needs` (dependencies).
  - Both `board/virt` (when booted as a Linux-style Image, the default) and `board/applem1` (via m1n1's converted FDT) build `dev.Node`s from the device tree in `x0` using `kernel/fdt` (with a static fallback only for ELF boots on `board/virt`).
- Drivers register by `compatible` from `init()` and implement `Run(ctx, *Device)`:
  - initialize, call `d.Ready(service)`, then keep servicing until `ctx` is done;
  - return nil when finished (a probe-only driver keeps its service published).
- `devmgr` (a process) supervises one `drv/<name>` process per device:
  - **dependencies:** a driver starts only after everything in `Needs` is ready, and fails if a dependency fails;
  - **restart:** an error or panic in *any* of the driver's goroutines restarts it with exponential backoff, up to `MaxRestarts` (3), then marks it `failed`;
  - **lookup:** `Manager.Get(ctx, name)` waits for readiness. Consumers find services by interface (`dev.Clock`, `dev.LatencyProbe`, `dev.Lister`).
- `Node.Config` carries settings the board resolved for the driver (from the device tree: power domains, pins, a PCI function), or the **error** that prevented it. The driver returns that error, so `lsdev` shows why instead of the device silently missing.
- **Bus drivers** add what they discover: `d.Add(Node)` registers a device and starts its driver under `devmgr`, like the boot-time ones (idempotent by name). `drivers/pci` adds one per function, compatible `pciclass,CCSSPP`, so drivers bind by class.
- `lsdev [NAME...]` shows every device (or those whose name starts with NAME), its state, PID, restarts, IRQs, registers and service info.

Drivers on QEMU virt today:

| Device | Driver | What it does |
|---|---|---|
| `gpio0` | `arm,pl061` | power button, interrupt-driven |
| `rtc` | `arm,pl031` | wall clock; the board also seeds Go's clock from it at boot |
| `virtio_mmio@…` | `virtio,mmio` | probe only: names a virtio device no driver claims |
| `rng0` | `virtio,device4` | virtio-rng: seeds the board's random generator (`crypto/rand`) at start, `Read` for more |
| `share0` | `virtio,device9` | virtio-9p: mounts QEMU's host share as the volume named by its tag (`host:`) |
| `usbdisk0` | `godeep,usb-storage` | a USB stick the xHCI driver found (behind hubs too): a `block.Device` + `block.Writer` (`READ(10)`/`WRITE(10)` + `REQUEST SENSE` recovery) |
| `usbnet0` | `godeep,usb-ecm` | USB CDC-ECM Ethernet adapter (`-device usb-net`, tested via `make check-usbnet`): publishes a `netdev.NIC` |
| `fbcon` | `godeep,fbcon` | the framebuffer console's state: size, history, scroll position, pointer, selection (QEMU: ramfb; the Mac: m1n1's framebuffer) |
| `net0` | `virtio,device1` | virtio-net: publishes a `netdev.NIC`; `netd` runs the TCP/IP stack on it (`ifconfig`, `sshd`). Interrupts, or polled under HVF |
| `ptimer` | `arm,armv8-ptimer-probe` | interrupt-latency probe |
| `pcie@10000000` | `pci-host-ecam-generic` | scans the bus, assigns BARs, adds `nvme0` and `xhci0` (QEMU adds a default PCI NIC only when no `-netdev` is given; the Makefile gives one, on virtio-mmio) |
| `nvme0` | `pciclass,010802` | polled NVMe (read & write); publishes a `block.Device`/`block.Writer` for `lsblk`, `ls`, `cat`, `write`, `cp` |
| `xhci0` | `pciclass,0c0330` | polled USB host; HID boot keyboards type into the console, mice publish `input` events (`evtest`), hubs/storage/CDC-ECM add child devices |

On the Mac image (`board/applem1`): `watchdog@2922b0000` (`apple,t6000-wdt`), `hid-transport@0` (`apple,spi-hid-transport`: the keyboard and trackpad), `idle` (the board's WFE idle statistics), and on QEMU only, `serial@…` (`arm,pl011` input).

## Input

Input drivers publish `input.Source`s (or `input.Sources` for a device with several, like Apple's keyboard + trackpad): events fan out through an `input.Broadcaster` that never blocks the driver (slow subscribers lose events, counted). Keyboards also type: they share `input.Keyboard`, which turns HID key state into console bytes on the channel the board hands them. There is no global event device; `evtest` finds sources among the device manager's services.

## Polling, for now

On the Mac there's no interrupt controller driver yet (AIC2), so drivers that would wait for an interrupt poll instead, and say so in `lsdev`:
- the keyboard and trackpad: the SPI ready line (a GPIO) every 5 ms;
- xHCI: the event ring and its fatal-error bit every 5 ms;
- NVMe spins briefly on the completion's phase bit, then sleeps 50 µs between checks;
- the board idles in WFE, woken by the timer's event stream about every 0.7 ms.

The cost is wake-ups, not busy CPU: one register read per poll, on wake-ups that mostly happen anyway. Each driver is written so the switch is local: "is there work?" becomes `<-line.C` instead of a timer.

### 4. `kernel/rtkit`: Apple coprocessor protocol

- Apple's coprocessors (SMC, NVMe/ANS, DCP, audio, ISP) run RTKit firmware and talk over a **mailbox**: 64-bit messages tagged with an endpoint.
- `rtkit.RTKit` is the protocol engine over a `Mailbox` interface. Its `Run` goroutine owns the mailbox and handles the management protocol:
  - version handshake (11–12)
  - endpoint discovery
  - starting system endpoints
  - power states
  - the firmware's **requests for shared buffers**, served from `kernel/dma` and mapped through the `IOMMU`
  - coprocessor crash detection through the crashlog endpoint
- Application endpoints become channels: `recv, send, err := r.Endpoint(ctx, ep)`. The SMC or NVMe driver just reads its channel.
- **Status:**
  - Tested against a fake coprocessor, including out-of-order messages, a crash and old firmware.
  - **The wire encodings follow Asahi Linux's `drivers/soc/apple/rtkit.c` and have not been checked on real hardware.**
  - The ASC mailbox hardware driver is still to do.

## Bugs found while building this

| Where | Bug | Fix |
|---|---|---|
| **TamaGo** `arm64/gic` | `DisableInterrupt` read-modify-writes `GICD_ICENABLER` (write-1-to-clear, reads back all enabled bits), so disabling one line disabled the whole bank of 32. The first keystroke silently disabled the power button. | `board/virt` writes the single bit. Worth reporting upstream. |
| godeep `board/virt` idle | The runtime passes `until = 0` for "no timer pending"; idle treated it as "already due" and returned, so with no timers the scheduler spun. | Treat 0 as "sleep up to `maxIdle`". |
| godeep `kernel/proc` | `pprof.Do` resets the *caller's* labels to the context's afterwards, so `Spawn` relabeled its caller as the new process. | A launcher goroutine sets labels for the child from birth (`goWithLabels`). |
| **QEMU 11.1.x HVF** | After the first device interrupt (SPI), every WFI returns immediately, and an idle VM burns 100% of a host core. Guest state is clean (`ISR_EL1=0`, GIC idle, timer armed); TCG is fine. Appears to be QEMU issue 959 ("Fix WFI halting to stop idle vCPU spinning"). | Under HVF, `board/virt` polls the UART (`UARTMode = "poll"`), so normal use raises no device interrupts. Timer interrupts don't trigger it. Revisit when Homebrew's QEMU includes the fix. |

## Where driver code lives

- **`drivers/<vendor>/<device>`** holds portable packages with no build tags, so they're host-tested. They take a `func(base) mmio.Regs` and return a `dev.Driver`: `drivers/arm/{pl011,pl031,pl061}`, `drivers/virtio/{mmio,net,rng,p9}`, `drivers/qemu/fwcfg`, `drivers/simplefb`, `drivers/pci`, `drivers/nvme`, `drivers/usb`, `drivers/usb/xhci`, `drivers/apple/{wdt,pmgr,gpio,spi,spihid}`.
  - Device-tree resolution that's specific to a driver lives with it and is host-tested (`spihid.ConfigFromFDT`, `pci.ConfigFromFDT`, `pmgr.FromFDT`); boards call it and put the result in `Node.Config`.
  - Tests model the hardware: an `mmio.Fake` with `OnRead`/`OnWrite` hooks (power domains that turn on, FIFOs, an NVMe controller executing commands from DMA memory, an SPI keyboard and trackpad sending packets and receiving the multitouch write, an xHCI controller with a USB keyboard and mouse).
- **`kernel/mmio`:** drivers use `Regs`.
  - On hardware it's a `Window`: single 32-bit atomic accesses.
  - In tests it's a `Fake` that models the device and records every access, so tests assert exact register sequences.
  - `mmio.Modify` (read-modify-write) is only for ordinary registers, never W1S/W1C banks.
- **Boards** register drivers and supply the device list. Both `board/virt` and `board/applem1` build that list from the boot device tree (`kernel/fdt`).

## Next drivers this enables

- **QEMU virt:** ✅ virtio-rng (real entropy), virtio-net (with gVisor's netstack and `sshd`, `docs/SSHServer.md`), virtio-9p (`host:`), `ramfb`/`simplefb`, PCIe/NVMe (read & write), and xHCI USB hubs, mass storage (read & write) and CDC-ECM Ethernet (`usb-net`). Next QEMU-testable and reusable on the Mac: SDHCI, virtio-gpu 2D.
- **M1:** Samsung debug UART (`drivers/apple/uart`), ASC mailbox (`rtkit.Mailbox`) and a hardware check of `kernel/rtkit`, then SMC (power-off, power button, lid, battery), the first RTKit client; then ANS2 (with SART) under `drivers/nvme`'s command layer; DART (`dma.IOMMU`); DWC3 USB-C gadget/host; AIC2 (`irq.Controller`) to replace polling.
