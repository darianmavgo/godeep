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
  - On M1 these will come from Apple's device tree (ADT); for now the virt board lists them statically.
- Drivers register by `compatible` from `init()` and implement `Run(ctx, *Device)`:
  - initialize, call `d.Ready(service)`, then keep servicing until `ctx` is done;
  - return nil when finished (a probe-only driver keeps its service published).
- `devmgr` (a process) supervises one `drv/<name>` process per device:
  - **dependencies:** a driver starts only after everything in `Needs` is ready, and fails if a dependency fails;
  - **restart:** an error or panic in *any* of the driver's goroutines restarts it with exponential backoff, up to `MaxRestarts` (3), then marks it `failed`;
  - **lookup:** `Manager.Get(ctx, name)` waits for readiness. Consumers find services by interface (`dev.Clock`, `dev.LatencyProbe`, `dev.Lister`).
- `lsdev` shows every device, its state, PID, restarts, IRQs, registers and service info.

Drivers on QEMU virt today:

| Device | Driver | What it does |
|---|---|---|
| `gpio0` | `arm,pl061` | power button, interrupt-driven |
| `rtc` | `arm,pl031` | wall clock; the board also seeds Go's clock from it at boot |
| `virtio` | `virtio,mmio-bus` | lists attached virtio devices |
| `ptimer` | `arm,armv8-ptimer-probe` | interrupt-latency probe |

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

- **`drivers/<vendor>/<device>`** holds portable packages with no build tags, so they're host-tested. They take a `func(base) mmio.Regs` and return a `dev.Driver`: `drivers/arm/pl061`, `drivers/arm/pl031`, `drivers/virtio/mmio`.
- **`kernel/mmio`:** drivers use `Regs`.
  - On hardware it's a `Window`: single 32-bit atomic accesses.
  - In tests it's a `Fake` that models the device and records every access, so tests assert exact register sequences.
  - `mmio.Modify` (read-modify-write) is only for ordinary registers, never W1S/W1C banks.
- **Boards** register drivers and supply the device list. `board/virt` builds that list from the boot device tree (`kernel/fdt`).

## Next drivers this enables

- **QEMU virt:** virtio-rng (real entropy) and virtio-net. Both are DMA ring devices: `kernel/dma` + bridge + device manager.
- **M1:** AIC2 (`irq.Controller`), DART (`dma.IOMMU`), ASC mailbox (`rtkit.Mailbox`), then SMC, the first RTKit client.
