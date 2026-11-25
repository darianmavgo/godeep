# Targeting my Mac: hardware, Go drivers, and a place to boot godeep

This doc covers everything about running godeep on the actual machine it's developed on, instead of in QEMU:
1. **the hardware** in this Mac;
2. **which existing Go drivers** can talk to each part;
3. **godeep's driver foundation:** the four kernel pieces already built for this hardware, and what each still needs on the M1;
4. **the TamaGo blockers** specific to this Mac;
5. **how godeep would boot** here;
6. **a step-by-step guide** to carving out a godeep boot entry and data partition next to macOS and Omarchy;
7. **how to develop native drivers incrementally** while QEMU, TamaGo and firmware cover whatever isn't native yet;
8. **the milestone order** for getting there.

**How the inventory was gathered (2026-11-25).** Everything came from the running machine through system tools; no web lookups were needed:
- `system_profiler` (hardware, displays, memory, NVMe, storage, Wi-Fi, Bluetooth, Thunderbolt, card reader, audio, camera, power, SPI)
- `sysctl` (CPU topology, caches, `hw.optional.arm.FEAT_*`)
- `diskutil list`, `diskutil apfs list`
- `ioreg -p IODeviceTree`: Apple's device tree. Its `compatible` strings are the names Linux and Asahi drivers bind to, so they're the key for finding driver code.

The serial number is deliberately left out of this file, since the repo may be shared.

---

## TL;DR

- **The machine:** a 14-inch MacBook Pro (2021), `MacBookPro18,1`, board `J316sAP`, **Apple M1 Pro** (SoC `t6000`). 8 performance + 2 efficiency cores, 16-core GPU, 16 GB LPDDR5, 512 GB SSD. macOS 26.6.2, plus Omarchy (Asahi-based Linux) on the same SSD.
- **Existing Go driver coverage on bare metal is thin.** The *CPU* side works: TamaGo's arm64 boot, MMU and timers, plus Go's assembly-accelerated crypto and SIMD. Almost every peripheral has no Go driver; the reference code is Asahi Linux (C/Rust) and its bootloader m1n1 (C/Python).
- **godeep now has its driver foundation** (§3): a DMA allocator with an IOMMU interface, an interrupt bridge, a device manager, and an RTKit coprocessor protocol engine. They're tested on the host and running on QEMU. On the M1 the remaining work narrows to three small hardware adapters: an AIC2 interrupt-controller driver, a DART IOMMU driver and an ASC mailbox driver. Every other M1 driver builds on those.
- **godeep now boots like a Linux kernel** (§3.1): a raw arm64 Image (`make image`), entry at **EL2** with a drop to EL1, and its device list read from the **device tree** the boot loader passes in `x0`. That's exactly what m1n1 does on this Mac, and it's tested in QEMU (`make qemu EL2=1`). Of the §4 blockers, two are done; the MMU patch and AIC2 remain.
- **Through QEMU (today's setup)**, godeep uses this Mac's CPU natively via HVF and reaches everything else through virtio. That remains the practical target.
- **For bare metal, this Mac is well placed.** The Omarchy install means Asahi's **m1n1** bootloader is already set up. §6 shows how to add a separate **godeep** boot entry and data partition between Macintosh HD and Omarchy without touching either. The first milestone is "godeep prints its shell on the laptop screen" via the framebuffer the firmware already set up, which needs **no display driver at all**.

---

## 1. Hardware inventory

### Machine

| Item | Value |
|---|---|
| Model | MacBook Pro, `MacBookPro18,1` (model number MK183LL/A) |
| Board ID (device tree) | `J316sAP` |
| OS | macOS 26.6.2 (Darwin 25.6) + Omarchy Linux (Asahi-based) on the same SSD |
| System firmware | 18000.161.10 |

### SoC, CPU, memory

| Component | Details | Device-tree `compatible` |
|---|---|---|
| SoC | Apple M1 Pro (`t6000`) | `arm-io,t6000` |
| Performance cores | 8 × Firestorm | `apple,firestorm` |
| Efficiency cores | 2 × Icestorm | `apple,icestorm` |
| ISA features | AES, PMULL, SHA3, DotProd, FP16, LSE atomics, PAuth, SSBS. **No** FEAT_RNG, SME, SVE, BF16, I8MM, MTE, BTI, WFxT, ECV | (`sysctl hw.optional.arm.*`) |
| Caches | L1D 64 KB, L2 4 MB (per cluster), 128 B cache lines; macOS page size 16 KB | |
| Memory | 16 GB LPDDR5 (Hynix). **DRAM base `0x100_0000_0000`**, size `0x4_0000_0000` | `/chosen dram-base`, `dram-size` |
| Memory controller | | `mcc,t6000` |
| Peripheral (MMIO) space | starts around `0x2_0000_0000` | `arm-io` ranges |

### Interrupts, timers, power and housekeeping

| Component | Details | `compatible` |
|---|---|---|
| Interrupt controller | **Apple AIC2** (not an ARM GIC) | `aic,2` |
| Timers | ARM generic timer (24 MHz) | (CPU architected) |
| Power manager | PMGR (per-device power and clock gates) | `pmgr1,t6000` |
| Watchdog | used for reboot | `wdt,t6000` |
| GPIO / pin control | 4 controllers | `gpio,t6000` |
| I²C | 6 buses (PASemi-style) | `i2c,t6000` |
| SPMI + PMU | power-management IC, RTC | `aapl,spmi`, `pmu,spmi` |
| SMC | battery, charging, sensors, power button, lid | `smc-pmu`, `smc-tempsensor` |
| Coprocessors | about 9 ARM coprocessors running Apple's RTKit firmware (SMC, NVMe, display, audio, …) | `iop,ascwrap-v4`, `iop-nub,rtbuddy-v2` |
| IOMMUs | DART IOMMUs in front of every DMA device | `dart,t6000`, `dart,t8110`, `sart,coastguard` |
| Hardware crypto | AES engine, Secure Enclave | `aes,s8000`, `iop-sep` |
| Boot / NVRAM flash | SPI NOR | `nor-flash,spi`, `nvram,nor` |
| Debug UART | Samsung-style UART (reachable through a USB-C port with Apple debug tooling) | `uart-1,samsung` |

### Storage

| Component | Details | `compatible` / bus |
|---|---|---|
| SSD | APPLE SSD AP0512R, 500 GB, NVMe-like **ANS2** controller ("Apple Fabric", not PCIe), behind RTKit + SART | (NVMExpress: "Apple SSD Controller") |
| Partitions | macOS APFS (357 GB, 75% unallocated), **Omarchy APFS stub (2.5 GB), `EFI - OMARC` (524 MB), 2 Linux filesystems (2.1 + 132 GB)**, Recovery (5.4 GB). Full table in §6 | `diskutil list` |
| SD card reader | Genesys Logic **GL9755** (PCIe SDHCI) | `pci17a0,9755` |

### Display, GPU, media

| Component | Details | `compatible` |
|---|---|---|
| Built-in display | Liquid Retina XDR, 3456×2234 | `disp0,t600x` |
| Display controller | DCP (runs RTKit firmware) | `dcp-expert-v1`, `display-crossbar,t600x` |
| External display | 2 × DisplayPort-over-Thunderbolt + HDMI via Parade **DP855** | `dispext0/1,t600x`, `parade,DP855` |
| GPU | Apple AGX, 16 cores, Metal 4 | `gpu,t6000` |
| Neural Engine | ANE | `ane,t8020` |
| Video | AVD decoder, AVE encoder, JPEG, scaler | `avd,t8103`, `ave2`, `jpeg,t6000`, `scaler,t6000` |

### Networking and wireless

| Component | Details | `compatible` / bus |
|---|---|---|
| Wi-Fi | Broadcom **BCM4387**, 802.11ax (Wi-Fi 6), PCIe | `wlan-pcie,bcm4387` |
| Bluetooth | Broadcom BCM4387, over PCIe | `bluetooth,n88` |
| Ethernet | none built in | |

### USB, Thunderbolt, PCIe

| Component | Details | `compatible` |
|---|---|---|
| USB-C ports | 3 × Thunderbolt 4 / USB4, 40 Gb/s | `acio`, `atc-phy,t6000` |
| USB controllers | DWC3 dual-role: **host or device** | `usb-drd,t6000` |
| USB-C power-delivery controllers | TI **CD3217** (TPS6598x family) on I²C | `usbc,cd3217` |
| PCIe | Apple PCIe root complex (Wi-Fi/BT and SD reader hang off it) | `apcie,t6000`, `apciec,t6000` |
| MagSafe | charging (140 W USB-C adapter attached) | |

### Input, audio, camera, biometrics, power

| Component | Details | `compatible` |
|---|---|---|
| Keyboard + trackpad | Apple internal, **HID over SPI** | `hid-transport,spi`, `spi-1,spimc` |
| Keyboard backlight | PWM | `fpwm,t6000` |
| Speakers | 6 × TI **TAS2764** amplifiers | `audio-control,sn012776` |
| Headphone jack | Cirrus Logic **CS42L84** | `audio-control,cs42l84` |
| Microphones | PDM array via the AOP coprocessor | `audio-aop-pdm2` |
| Audio plumbing | MCA (I²S), ADMAC DMA | `mca,t600x`, `admac,t600x` |
| Camera | FaceTime HD via ISP | `isp,h13-generic` |
| Touch ID | sensor tied to the Secure Enclave | `biosensor,mesa` |
| Battery | TI **bq40z651** fuel gauge, 472 cycles, condition Normal (read via SMC) | |
| Temperature sensors | TI TMP103 + SMC sensors | `temperature,tmp103` |

---

## 2. Which Go code can talk to it

Legend:
- ✅ works as-is
- ◐ partial: Go code exists for part of the job (often godeep's own foundation, §3) and the rest is a small adapter
- ✗ no Go driver exists; port from the reference code

Effort is what's **left**: XS = hours, S = days, M = 1–3 weeks, L = 1–3 months, XL = a project of its own. "Reference" is what you'd port from.

### CPU-level (where Go is strong)

| Hardware | Go code | Status | Notes / reference | Effort |
|---|---|---|---|---|
| CPU boot, exceptions, caches | `tamago/arm64` + godeep's own entry (`board/virt/entry_arm64.s`) | ◐ | **EL2 entry done** (§3.1, tested at EL2 in QEMU). Left: the MMU reaching RAM at `0x100_0000_0000` | S |
| MMU / page tables | `tamago/arm64` mmu | ◐ | Hard-codes a 39-bit VA with flat mapping. Needs a 48-bit VA (T0SZ=16), 4-level tables and 16 KB-aware DMA mappings | S–M |
| Generic timer | `tamago/arm64` `InitGenericTimers`, `GetTime` | ✅ | Architected and identical to QEMU. The timer *interrupt* arrives as a FIQ through AIC, not the GIC | — |
| AES, SHA-1/2/3, GHASH | Go std `crypto/*` (arm64 assembly) | ✅ | Uses the CPU's AES, SHA3 and PMULL instructions directly | — |
| NEON SIMD | Go `simd`, `simd/archsimd` (GOEXPERIMENT=simd) | ✅ | Already enabled in the Makefile | — |
| Atomics | Go `sync/atomic` (LSE) | ✅ | | — |
| Random numbers | `board/virt` software DRBG | ◐ | No FEAT_RNG on M1 Pro, and the SEP TRNG isn't accessible. Seed from the firmware's `/chosen/random-seed` (iBoot provides it at boot; m1n1 passes it on) | XS |
| More cores (SMP) | none (TamaGo has no arm64 SMP) | ✗ | Apple starts cores through PMGR "cpu-start" registers, not PSCI. Reference: m1n1 `smp.c`, Linux `arch/arm64` + Apple PMGR | M |
| **Interrupt controller (AIC2)** | **godeep `kernel/irq` bridge** (routing, oneshot masking, stats, latency probe) | ◐ | Only the controller adapter is left: implement `irq.Controller` (`Enable`/`Disable`) plus the ack loop for AIC2's event register. Reference: Linux `irq-apple-aic.c`, m1n1 `aic.c` | S |
| Power off / reboot | `board/virt` PSCI | ✗ | No PSCI firmware on Apple Silicon. Reboot through the watchdog (Linux `apple_wdt.c`, tiny); power-off through SMC. The kernel's shutdown path (`Board.PowerOff`) is ready | XS / M |
| **Device discovery + driver supervision** | **godeep `kernel/dev`** device manager + **`kernel/fdt`** parser | ◐ | Drivers run as supervised, restartable processes, and the device list comes from the FDT in `x0` (§3.1). m1n1 converts Apple's ADT into an FDT for its payload, so no ADT parser is needed. Left: check m1n1's FDT for this Mac, and map AIC2's interrupt cells | XS |

### Peripherals

| Hardware | Go code | Status | Reference to port | Effort |
|---|---|---|---|---|
| **Boot framebuffer** (screen, as set up by iBoot/m1n1) | none needed: plain memory + Go `image/draw`, `golang.org/x/image/font` | ✅ | Address/size/stride come from m1n1's boot args (the same thing Linux's `simplefb` uses) | XS–S |
| Debug UART (Samsung) | none | ✗ | Linux `samsung_tty.c`, m1n1 `uart.c`; ~100 lines. Needs Apple debug-USB tooling or a second Apple Silicon Mac (`macvdmtool`) to reach | XS |
| Watchdog | none | ✗ | Linux `apple_wdt.c` | XS |
| GPIO / pin control | none | ✗ | Linux `pinctrl-apple-gpio.c` | S |
| I²C | none on bare metal (`periph.io` drivers need a host OS) | ✗ | Linux `i2c-pasemi-*.c` | S |
| PMGR power gates | none | ✗ | Linux `apple-pmgr-pwrstate.c` | S |
| **DART IOMMU** | **godeep `kernel/dma`**: allocator outside the Go heap (16 KB alignment, ownership, stats) + the `dma.IOMMU` interface drivers already call | ◐ | Only the DART page tables and registers are left: implement `Map`/`Unmap`. Reference: Linux `apple-dart.c`, m1n1 `dart.c` | S–M |
| **RTKit coprocessors** | **godeep `kernel/rtkit`**: handshake, endpoint discovery, power states, shared-buffer requests (via `kernel/dma`), crash detection, per-endpoint channels | ◐ | Left: the ASC mailbox hardware driver (implements `rtkit.Mailbox`), and checking the message encodings on real hardware. Reference: Linux `apple-mailbox.c`, `drivers/soc/apple/rtkit*.c`, m1n1 `rtkit.c` | S |
| SMC (battery, sensors, lid, power button) | rides on `kernel/rtkit` | ✗ | The SMC protocol on top of an RTKit endpoint. Asahi `macsmc` | S–M |
| **Keyboard / trackpad** (HID over SPI) | none (Go HID libraries wrap host hidapi) | ✗ | Linux `spi-apple.c` + Asahi's SPI-HID transport; HID report parsing | M |
| **SSD** (ANS2) | rides on `kernel/rtkit` + `kernel/dma` | ✗ | The NVMe-like queues on top of RTKit + SART. Linux `drivers/nvme/host/apple.c` | M |
| Filesystems on the SSD | ext4 / APFS readers exist as host Go libraries | ◐ | After NVMe works. Read-only ext4 of the Omarchy partition is the realistic target; never write to APFS | M |
| PCIe root complex | `tamago/soc/intel/pci` is x86 port I/O, doesn't apply | ✗ | Linux `pcie-apple.c`. m1n1 can bring the links up before handing over | M |
| SD reader (GL9755, SDHCI) | `tamago/soc/nxp/usdhc` (different controller, same SD protocol) | ◐ | Linux `sdhci-pci-gli.c`; reuse the SD command logic from usdhc | M |
| **USB-C in device mode** (gadget) | `usbarmory/go-net/imx-usb` (on top of `go-net`): CDC-ECM Ethernet-over-USB, written for NXP's controller | ◐ | Needs a Go DWC3 *device-mode* driver (Linux `dwc3`). Then the existing CDC-ECM + gVisor stack gives **networking over a USB-C cable**. m1n1 uses the same port this way | M–L |
| USB host (DWC3 xHCI) | none | ✗ | Linux `xhci`, `dwc3`; large | L |
| USB-C PD controller (CD3217) | none | ✗ | Linux `tipd` driver; needed to switch port roles and modes | M |
| Thunderbolt / USB4 | none | ✗ | Asahi work in progress | XL |
| **Wi-Fi** (BCM4387) | none | ✗ | Linux `brcmfmac` plus the firmware blobs the Asahi installer copies from macOS | XL |
| Bluetooth (BCM4387) | none | ✗ | Linux `hci_bcm4377.c` | L |
| Network stack (TCP/IP) | **gVisor netstack via `usbarmory/go-net`** | ✅ | Works on any frame source: USB gadget Ethernet, virtio-net in QEMU | — |
| Display controller (DCP) | none (would ride on `kernel/rtkit`) | ✗ | Asahi DCP driver. Use the boot framebuffer instead | XL |
| External displays / HDMI | none | ✗ | DCP + Parade DP855 + Type-C alt modes | XL |
| GPU (AGX) | none | ✗ | Asahi's Rust `drm/asahi` + Mesa. See `../GoGpuSupport.md` | XL+ |
| Neural Engine | none | ✗ | Reverse-engineered, unofficial | XL+ |
| Video encode/decode, JPEG, scaler | none | ✗ | Partial Asahi work | L–XL |
| Speakers, headphones, mics | none (audio coprocessor would ride on `kernel/rtkit`) | ✗ | Asahi `macaudio`, `mca`, `admac`, TAS2764, CS42L84 drivers | L |
| Camera (ISP) | none | ✗ | Asahi ISP driver, firmware-dependent | XL |
| Touch ID / Secure Enclave | none | ✗ | Closed SEP firmware; not feasible | — |
| AES engine / SEP crypto | none | ✗ | Not needed: the CPU's crypto instructions (✅) are faster for godeep | — |
| Boot NOR flash / NVRAM | none | ✗ | **Don't touch**: writing here can make the Mac unbootable | — |
| Battery / charging info | none | ✗ | Comes free once SMC works | S (after SMC) |
| Keyboard backlight (PWM) | none | ✗ | Linux `pwm-apple.c`; trivial | XS |

**Scorecard (updated after the foundation work):**
- **5 blocks work with existing Go code:** timer, crypto, SIMD, atomics, and the boot framebuffer through Go's `image` packages. The gVisor network stack also works once something supplies frames.
- **9 are partial (◐).** Four of them are now mostly done because of godeep's own foundation (§3): interrupt controller, DART IOMMU, RTKit coprocessors and device discovery. The other five come from outside Go code: TamaGo boot/MMU, the RNG, ext4 readers, uSDHC for SD and go-net/imx-usb for USB Ethernet.
- **The RTKit-based drivers (SMC, SSD, audio, display) shrink** to "the protocol on top of an endpoint": the mailbox handshake, power and shared buffers are already written.
- **Everything else is still a port.** Asahi Linux remains the only complete reference for this hardware, and it's all open source.

---

## 3. godeep's driver foundation (built, 2026-11-25)

Almost every device in §1 needs the same four things: memory it can DMA into, a way to receive interrupts, a place in a startup order with its dependencies, and, for about 9 of them, the RTKit mailbox protocol. These were built first, as portable kernel packages:
- tested on the host, 50× under the race detector;
- running on QEMU arm64 + HVF.

Design and rules are in [`DriverModel.md`](DriverModel.md).

| Piece | Package | What it gives every M1 driver | Running on QEMU now | M1 adapter still needed |
|---|---|---|---|---|
| DMA memory + IOMMU | `kernel/dma` | buffers outside the Go heap with stable physical addresses, 16 KB alignment for DART, single ownership (double free panics), stats; `dma.IOMMU` so drivers hand devices an IOVA | 512 MB region at `0x1_2000_0000` (`dmastat`); `dma.Identity` IOMMU | **DART** implementing `Map`/`Unmap` |
| Interrupt bridge | `kernel/irq` | interrupts arrive as channel events; the line stays masked until the driver calls `Done` (oneshot, safe for level-triggered lines); per-line counts and service time | GICv3: power button + timer probe (`irqstat`, `irqlat`) | **AIC2** implementing `irq.Controller` + its ack loop |
| Device manager | `kernel/dev` | each driver is a process `drv/<name>` supervised by `devmgr`: dependency ordering (`Needs`), readiness (`Ready`/`Get`), restart with backoff when any driver goroutine panics | 4 drivers: PL061 power button, PL031 RTC, virtio-mmio scanner, timer probe (`lsdev`) | **ADT parser** producing `dev.Node`s from m1n1's device tree |
| RTKit coprocessors | `kernel/rtkit` | version handshake (11–12), endpoint map, starting system endpoints, IOP power states, the firmware's shared-buffer requests (allocated from `kernel/dma`, mapped through the IOMMU), crash detection, one channel per application endpoint | protocol engine tested against a fake coprocessor (out-of-order messages, crash, old firmware); QEMU has no RTKit device | **ASC mailbox** driver implementing `rtkit.Mailbox`; **verify the message encodings** on the real Mac (they follow Asahi's `rtkit.c`) |

**Measured on this Mac (QEMU + HVF):**

| What | Value |
|---|---|
| Interrupt → driver goroutine, CPU awake | median **~11 µs** (exception, TamaGo signal relay, `irq` thread, bridge, scheduler) |
| Same, CPU asleep in WFI | ~300 µs, which is the host waking the vCPU thread; real hardware wakes in microseconds |
| Bridge service time | ~10–27 µs |
| Idle host CPU | ~2% under HVF (UART polled, see below), ~0.2% under TCG |

### 3.1 Booting like a Linux kernel (built, 2026-11-25)

m1n1, U-Boot's `booti` and QEMU's `-kernel` all load kernels the same way: a raw arm64 **Image** at a 2 MB-aligned RAM base + `text_offset`, entered at its first instruction with the **device tree address in `x0`**. On this Mac, m1n1 enters at **EL2**. godeep now does all of this, and it's tested in QEMU:

| Piece | Where | What it does | Tested |
|---|---|---|---|
| `make image` | `tools/mkimage` | turns the TamaGo ELF into an arm64 Linux Image: 64-byte header (branch to entry, `text_offset`, size incl. bss, magic `ARM\x64`) over the never-executed ELF header, so no code moves | host test on the real kernel; QEMU boots it (now the default `make qemu`) |
| EL2 → EL1 entry | `board/virt/entry_arm64.s` (TamaGo `linkcpuinit` build tag replaces its `cpuinit`) | at EL2: `HCR_EL2.RW`, EL1 timer access (`CNTHCTL_EL2`), no FP/cp15 traps, `ICC_SRE_EL2.Enable` for the GIC, then `ERET` to EL1 with interrupts masked | `make qemu EL2=1`: QEMU TCG with `virtualization=on` enters at EL2 like m1n1; full shell, GIC, timers and power-off work |
| device tree capture | same file | keeps `x0`, validates the FDT magic, copies it (up to 2 MB) to the start of the DMA region **before the Go heap exists**, since the loader may have put it where the heap will grow | QEMU Image boot |
| device list from the FDT | `kernel/fdt` (parser with `ranges` translation for `/soc`-style buses, GIC interrupt cells) + `board/virt/devices.go` | every node with a `compatible` becomes a `dev.Node`; empty virtio slots are skipped; nodes without a driver show as `no driver` in `lsdev` (like `lspci`); `/psci` `method` picks the power-off conduit (`hvc`/`smc`) | host tests against QEMU's real DTB; `lsdev` lists 10 devices from the tree |
| register access | `kernel/mmio` (`Regs`; `Window` on hardware, recording `Fake` in tests; `Modify` documented as never-for-W1C) | §7.2 rung 2 | host tests |
| portable drivers | `drivers/arm/pl061` (power button), `drivers/arm/pl031` (RTC), `drivers/virtio/mmio` (slot probe) | moved out of the board; host-tested against `mmio.Fake`, e.g. the power button's config writes touch only its own bit | host tests + QEMU |

The first boot line says how godeep was entered, for example:

```
board: qemu-virt; entered at EL2, dropped to EL1; linux,dummy-virt, device tree from boot loader (10 devices); PSCI via smc
```

**Lessons from building it that carry over to the M1:**
- **Never read-modify-write a write-1-to-set/clear register.** TamaGo's GIC driver did exactly that on `GICD_ICENABLER`: disabling one interrupt disabled its whole bank, and the first keystroke silently disabled the power button. AIC2's mask registers have the same semantics, so its adapter must write single bits.
- **`goos.Idle(until)` gets `until == 0` for "no timer pending",** not "due now". Treating it as due spun the CPU.
- **Device registers: single 32-bit atomic loads and stores.** That's what hypervisors (and m1n1's hypervisor mode) can decode.
- **QEMU 11.1.x HVF WFI bug.** After the first device interrupt, idle vCPUs spin (QEMU issue 959). Guest state is clean, and TCG and real hardware are unaffected. `board/virt` polls the UART under HVF to avoid it.
- **Running tests 50× under the race detector found most of the real bugs.** Keep doing it for M1 drivers.

---

## 4. TamaGo blockers specific to this Mac

These must be fixed before godeep can print a single character on the real hardware:

1. ✅ **Entry at EL2.** *Done (§3.1): `board/virt/entry_arm64.s` drops EL2 → EL1 and is tested at EL2 in QEMU. `board/applem1` reuses it, adding any Apple-specific EL2 setup m1n1's docs call for.* Background: m1n1 starts payloads at **EL2** (Asahi kernels run in EL2/VHE). `tamago/arm64` `cpuinit` handles EL1 or EL3 only: from EL2 it would execute EL3 register writes, which is an undefined-instruction fault. Add an EL2→EL1 drop (set `HCR_EL2.RW`, then `ERET` to EL1). That's about 15 lines of assembly in a board-specific `linkcpuinit` override.
2. **RAM is above 1 TiB.** DRAM starts at `0x100_0000_0000`. TamaGo's arm64 MMU sets up a 39-bit VA with a 40-bit physical range and flat mapping, which can't reach it. Either patch the MMU (48-bit VA, 4-level tables) or map RAM at a low virtual address. Flat mapping plus a 48-bit VA is the simpler patch. The DMA region (§3) must sit in RAM the Go heap doesn't own, exactly as `board/virt` does it.
3. **AIC, not GIC.** `board/virt`'s idle routine and interrupt service use the GIC. On the M1, the `irq.Controller` adapter talks to AIC2, and the timer FIQ wakes WFI the same way.
4. **No PSCI.** `halt` and `make stop`'s power-off must use the watchdog or SMC instead (`Board.PowerOff`). The board already reads the PSCI conduit from the device tree; on the M1 the tree has no `/psci`, which the board will use as the cue to use the watchdog.
5. ✅ **Image format.** *Done: `make image` (`tools/mkimage`) builds a raw arm64 Image, and QEMU boots it by default.* For the M1, link `board/applem1` at the RAM base m1n1 uses and set `-ram` to match.
6. **16 KB IOMMU pages.** DART works in 16 KB pages, so DMA buffers must be 16 KB-aligned (`dma.Alloc(size, 16384)`) even if the CPU uses 4 KB pages.

---

## 5. How godeep would boot on this Mac

Thanks to the Omarchy (Asahi-based) install, Asahi's boot chain already exists:

```
iBoot (Apple)  →  m1n1 stage 1 (in the "Omarchy" APFS stub)
               →  m1n1 stage 2 + device tree (on "EFI - OMARC")  →  U-Boot  →  GRUB/Linux
```

Ways to slot godeep in, safest first:

| Option | How | Needs | Risk |
|---|---|---|---|
| **A. m1n1 proxy + hypervisor** (dev loop) | Reboot into m1n1 proxy mode and push godeep over USB-C with m1n1's `proxyclient` tools (`chainload.py` / `run_guest.py`). In hypervisor mode, m1n1 runs godeep as a guest and **logs every hardware access**, which is the ideal way to debug new drivers | A **second computer** connected over USB-C | Nothing is written to disk |
| B. Omarchy's U-Boot | Copy `godeep.img` to the `EFI - OMARC` partition and boot it from the U-Boot prompt (`booti`) | Only this Mac | Low, but you're editing Omarchy's boot partition, so back it up first |
| **C. Its own boot entry** (recommended) | A separate godeep stub + ESP + data partition, created with the Asahi installer between Macintosh HD and Omarchy. **Step by step in §6** | Only this Mac | Low; leaves macOS and Omarchy untouched; uses about 12 GB |

Never write to the macOS container, the APFS stub internals, NOR flash or the Recovery partition. Holding the power button at boot opens the startup picker, which always gets you back.

---

## 6. Carving out a godeep partition next to Macintosh HD and Omarchy

This sets up option C: a bootable **godeep** entry in the startup picker plus an optional data partition, physically between Macintosh HD and Omarchy, leaving both untouched.

> **godeep cannot boot on this Mac yet.** This prepares the place it will boot from. Running on the real hardware still needs the §4 blockers fixed and the §3 M1 adapters. Until then the new entry starts U-Boot and stops at its prompt, which is the expected result.

You run every command here yourself; nothing in this section was run by Claude.

### 6.1 What you'll end up with

Today (from `diskutil list disk0`; the disk is 500.3 GB and has **no free space**):

```
disk0
 s1  Apple_APFS_ISC        524 MB   system firmware             ── never touch
 s2  Apple_APFS (disk4)  356.9 GB   Macintosh HD                ── 267.7 GB of it unused
 s3  Apple_APFS (disk2)    2.5 GB   Omarchy stub (m1n1)         ── never touch
 s4  EFI                   524 MB   EFI - OMARC                 ── never touch
 s5  Linux Filesystem      2.1 GB   Omarchy /boot               ── never touch
 s6  Linux Filesystem    132.3 GB   Omarchy root                ── never touch
 s7  Apple_APFS_Recovery   5.4 GB   system recoveryOS (last)    ── never touch
```

After (with the default 12 GB):

```
 s1  Apple_APFS_ISC        524 MB   system firmware
 s2  Apple_APFS          344.9 GB   Macintosh HD        (12 GB smaller)
 new Apple_APFS            2.5 GB   godeep stub (m1n1)  ┐
 new EFI                   500 MB   EFI - GODEE…        │ godeep, between
 new MS-DOS FAT32         ~9 GB     GODEEPDATA          ┘ macOS and Omarchy
 s3  Apple_APFS            2.5 GB   Omarchy stub
 s4  EFI                   524 MB   EFI - OMARC
 s5  Linux Filesystem      2.1 GB   Omarchy /boot
 s6  Linux Filesystem    132.3 GB   Omarchy root
 s7  Apple_APFS_Recovery   5.4 GB   system recoveryOS
```

The space comes from shrinking Macintosh HD's container, which is 75% unallocated, so the freed gap lands right after `s2` and before Omarchy's `s3`.

New partitions get the next free *entry numbers* (`disk0s8`, `disk0s9`, …) even though they sit physically between `s2` and `s3`. Always go by **name and size**, never by number alone.

**Choosing the size.** The godeep stub (2.5 GB) and its ESP (0.5 GB) need about 3 GB. The rest becomes GODEEPDATA, for godeep's future disk driver and filesystem work (`../ReUsingGVisor.md` §5) and as a drop box shared with macOS and Omarchy. 12 GB gives about 9 GB of data space. Use 4 GB if you only want the boot entry.

### 6.2 Safety first

- **Back up first** (Time Machine or similar). Resizing APFS is routine, but it's still repartitioning the only disk in the machine.
- **Never touch** `disk0s1` (ISC), `disk0s3`–`disk0s6` (Omarchy), `disk0s7` (system recoveryOS, which must stay the last partition), or anything outside `disk0`.
- **Plug in the power adapter** for the whole process.
- **FileVault is on.** That's fine: you resize while logged in, so the volume is unlocked.
- **How to get back to macOS at any point:** shut down, then hold the power button until "Loading startup options" appears, and pick Macintosh HD. Choose "Options" in the same picker to get recoveryOS.
- **Check Omarchy still boots before you start**, so you'll know if anything changes.

### 6.3 Pre-flight checks

```sh
diskutil list disk0                         # must match the "Today" table above
diskutil apfs list disk4 | grep -E "Capacity Ceiling|Not Allocated"
#   Size (Capacity Ceiling):  356945842176 B (356.9 GB)
#   Capacity Not Allocated:   267714056192 B (267.7 GB) (75.0% free)
tmutil listlocalsnapshots /                 # only com.apple.os.update-* is fine
sudo diskutil apfs resizeContainer disk4 limits   # preview only, changes nothing
```

`limits` prints the minimum size the container can shrink to. Your target, `344.9g` with the defaults, must be above that minimum. It will be, by a very wide margin. If you see `com.backupd` (Time Machine) local snapshots, thin them first: `tmutil thinlocalsnapshots / 999999999999 4`.

### 6.4 Step 1: shrink Macintosh HD

New size = **current 356.9 GB minus your godeep size.** With the 12 GB default that's `344.9g`:

```sh
sudo diskutil apfs resizeContainer disk4 344.9g
```

`disk4` is Macintosh HD's container (the form `diskutil` prefers; it equals the physical store `disk0s2`). The resize runs live and takes a minute or two. Then run `diskutil list disk0`: a `(free space)` line of about 12 GB should appear between the Macintosh HD container and Omarchy's container. Don't create anything in it by hand: the next step needs it empty.

### 6.5 Step 2: create the bootable godeep stub (Asahi installer)

On Apple Silicon a boot entry needs **a small APFS "stub" container** holding a boot loader, registered with Apple's boot policy from recoveryOS. Doing that by hand isn't practical. Omarchy already used the Asahi installer for this, and the same tool can add a second, independent entry.

```sh
curl https://alx.sh | sh
```

This is the official Asahi Linux installer: it downloads and runs as root after you confirm, so read its prompts. The menu wording changes between versions; look for these choices:

1. **Install an OS** (not "Resize" since you already made free space; not anything that mentions Omarchy).
2. Use the **free space** it reports (about 12 GB).
3. Choose **"UEFI environment only (m1n1 + U-Boot + ESP)"**. This installs just the boot chain: no Linux, no desktop.
4. When asked for a name, enter **`godeep`**. That's the label shown in the startup picker.
5. When asked how much space to use, give it **only what it needs** (it typically suggests the minimum, about 3 GB). Leave the rest free for GODEEPDATA.

The installer then tells you to **shut down and finish in recoveryOS**:
- Wait 15 seconds after shutting down.
- Hold the power button until "Loading startup options" appears.
- Pick the new **godeep** entry and follow the on-screen steps. It asks for your macOS user password, to approve the boot policy for the new entry.

When it finishes, the Mac reboots into the new entry, which runs m1n1 → U-Boot.

> **For driver development:** the installer also offers **"Tethered boot (m1n1, for development)"** (option A in §5). That entry waits for a second computer over USB-C, which can push kernels and run them under m1n1's hypervisor with every hardware access logged. It's ideal for writing the §3 M1 adapters; add it alongside the same way.

### 6.6 Step 3 (optional): the GODEEPDATA partition

Back in macOS, find the new godeep ESP: the **EFI** partition of about 500 MB that is *not* `EFI - OMARC`.

```sh
diskutil list disk0
# e.g.  9:  EFI EFI - GODEE…   500.2 MB   disk0s9     ← note this identifier
sudo diskutil addPartition disk0s9 FAT32 GODEEPDATA 0    # use YOUR godeep ESP id
```

`0` means "fill the free space". FAT32 is the first filesystem godeep will learn to read and write, and macOS, Omarchy and godeep can all use it.

### 6.7 Step 4: verify

Run `diskutil list disk0` and check that:
- Macintosh HD's container is `344.9 GB`.
- A new ~2.5 GB APFS container, a ~500 MB EFI and GODEEPDATA appear **between** it and Omarchy's 2.5 GB container. `diskutil info <id>` shows each one's offset if you want to confirm the physical order.
- `disk0s3`–`disk0s7` are unchanged.

The startup picker (hold the power button at boot) should show **Macintosh HD, Omarchy and godeep**:
- **godeep** reaches the **U-Boot** prompt, or U-Boot's boot menu. That's success for now.
- **Omarchy** still boots normally.

### 6.8 Step 5 (future): put godeep on its ESP

This becomes possible once godeep has an M1 board (`board/applem1`) and a `make image` target producing a raw arm64 Image (§4 #5):

```sh
sudo diskutil mount disk0s9                     # the godeep ESP
cp bin/godeep-applem1.img /Volumes/EFI\ -\ GODEE*/godeep.img
diskutil unmount disk0s9
```

Boot the godeep entry and, at the U-Boot prompt:

```
=> load nvme 0:<godeep-ESP-partition> ${kernel_addr_r} godeep.img
=> booti ${kernel_addr_r} - ${fdtcontroladdr}
```

U-Boot's partition numbering differs from macOS's `diskN`; `part list nvme 0` shows it. Once this works, a `boot.scr` script or EFI boot entry can make it automatic.

### 6.9 Undo: remove godeep and give the space back

1. Find the godeep stub container (the ~2.5 GB APFS container that is **not** Omarchy's `disk2`):

   ```sh
   diskutil apfs list | grep -B2 -A12 "godeep"
   ```

2. Delete godeep's three partitions. **Double-check each identifier is godeep's, not Omarchy's:**

   ```sh
   sudo diskutil apfs deleteContainer <godeep stub container, e.g. disk7>
   sudo diskutil eraseVolume free none <godeep ESP, e.g. disk0s9>
   sudo diskutil eraseVolume free none <GODEEPDATA, e.g. disk0s10>
   ```

3. Grow Macintosh HD back into the freed space:

   ```sh
   sudo diskutil apfs resizeContainer disk4 0     # 0 = grow to fill
   ```

The godeep entry disappears from the startup picker once its stub container is gone.

### 6.10 Troubleshooting

| Problem | What to do |
|---|---|
| `resizeContainer` refuses ("not enough free space", "snapshots") | Run `limits` again; thin Time Machine local snapshots (§6.3); reboot and retry, since a pending macOS update can pin space |
| The installer says there's no free space | Make sure Step 1 left a `(free space)` gap (`diskutil list disk0`), and that you didn't create a partition in it |
| No "godeep" in the startup picker | The recoveryOS step didn't complete. Boot it from the picker ("Options" → the godeep entry) and finish the installer's prompts |
| Booting godeep shows a black screen or a U-Boot prompt | Expected until godeep can boot on the metal |
| Omarchy stopped booting | Nothing here touches its partitions. Pick Omarchy explicitly in the startup picker. If it still fails, its own recovery steps apply; your backup covers the rest |
| Want macOS back immediately | Hold the power button → pick Macintosh HD |

---

## 7. Developing native drivers incrementally

The goal is to replace about 40 hardware blocks with native Go drivers **one at a time**, without ever losing a working system. At every step, godeep should boot and be useful, with whatever isn't native yet covered by QEMU, a TamaGo or go-net driver, or state the firmware already set up.

### 7.1 The principle: one kernel, swappable boards, drivers behind interfaces

godeep is already shaped for this:

```
             ┌──────────────── kernel/ (identical everywhere) ────────────────┐
             │ proc · tty · sh · dev (manager) · irq (bridge) · dma · rtkit   │
             └──────▲─────────────▲──────────────▲──────────────▲─────────────┘
                    │ interfaces: irq.Controller · dma.IOMMU · rtkit.Mailbox ·
                    │ kernel.Console · dev.Clock · dev.LatencyProbe · …
     board/hosted   board/virt (QEMU, HVF/TCG)   board/microvm   board/applem1 (future)
     host OS shims  GIC · PL011 · PL031 · PL061   TamaGo microvm  AIC2 · DART · ASC ·
                    · virtio · Identity IOMMU                    Samsung UART · …
```

- **The kernel never knows which board it's on.** A board supplies a `kernel.Board`: console, interrupt bridge, DMA region, device list, power-off.
- **Drivers bind by `compatible` string** through `dev.Register`, so a board's device list decides which drivers run.
- **Consumers ask for services, not drivers.** The shell asks the device manager for "a `dev.Clock`", not "the PL031". Whether that's QEMU's RTC or the M1's SMC RTC, nothing above the driver changes.

**So "partially native" is a supported state.** `board/applem1` can start with three real drivers and borrow everything else, and the same kernel keeps running on `board/virt` for everything not yet ported.

### 7.2 The development ladder for each driver

Every native driver climbs the same five rungs. Each rung is cheap to rerun, and most bugs die on the first two, where iteration takes seconds.

| Rung | Where | What you test | Tools |
|---|---|---|---|
| **1. Protocol on the host** | `go test` on macOS | The driver's logic as pure Go against a **fake device**: message formats, state machines, queue handling, error and crash paths | the way `kernel/rtkit` is tested against a fake coprocessor; `go test -race -count=50` |
| **2. Registers against a model or trace** | `go test` on macOS | Register sequences, through an MMIO interface whose fake records reads and writes and replays **real traces captured on the Mac** (rung 4) | proposed `kernel/mmio` package (below); m1n1 hypervisor trace logs |
| **3. Integration on a QEMU analog** | `make qemu` (HVF) | The driver's *shape* inside godeep: a `drv/<name>` process under `devmgr`, interrupts through the bridge, buffers from `kernel/dma`, restart on crash, idle CPU | the closest QEMU device (table below) |
| **4. Real hardware under m1n1's hypervisor** | the Mac, from a second computer (§5 option A) | The real device, with **every MMIO access logged**. godeep runs as a guest, and m1n1 can stop, inspect and trace it | m1n1 `run_guest.py`, the tethered-boot entry (§6.5) |
| **5. Native boot** | the godeep boot entry (§6) | Boots without a second computer: the driver is done | U-Boot `booti` (§6.8) |

QEMU analogs for rung 3 (same role, same godeep plumbing, different registers):

| M1 hardware | QEMU stand-in that exercises the same path |
|---|---|
| AIC2 interrupt controller | GICv3 (`board/virt`): both implement `irq.Controller` |
| DART IOMMU | `dma.Identity`, or a fake IOMMU that remaps (useful to catch drivers that hand devices `Phys` instead of the IOVA) |
| ASC mailbox + RTKit | a fake coprocessor goroutine behind `rtkit.Mailbox` (as in the tests), run inside the kernel as a pretend device |
| Samsung UART | PL011 |
| ANS2 NVMe | virtio-blk (submission/completion queues in DMA memory, doorbells, interrupts) |
| DWC3 USB gadget + CDC-ECM | virtio-net (frames in and out of the gVisor stack) |
| SMC (battery, lid, power button) | PL061 power button + PL031 RTC (events and a clock through the same service interfaces) |
| Boot framebuffer | `ramfb` / virtio-gpu (`../GoGpuSupport.md`) |

### 7.3 Borrow what isn't native yet

While a block has no native driver, something else covers it. Each row says what stands in, and what retires it.

| Need | Stand-in until the native driver lands | Retired by |
|---|---|---|
| Everything (daily development) | **QEMU `board/virt`**: virtio net/blk/9p/rng/gpu, PL011, RTC | each native driver, one at a time; QEMU stays for tests and CI |
| CPU boot, MMU, timer, caches | **TamaGo `arm64`** (with the §4 patches) | stays: this *is* the native driver |
| TCP/IP, TLS, SSH | **gVisor netstack via `go-net`**, `x/crypto/ssh` | stays: only the NIC underneath changes |
| Screen | **firmware framebuffer** that iBoot/m1n1 already set up | DCP driver (maybe never: the framebuffer is enough) |
| Console | framebuffer text out; **Samsung debug UART** in/out via a second Mac or the m1n1 proxy | SPI-HID keyboard (M2) |
| PCIe links, some clocks and power domains | **m1n1** brings them up before jumping to godeep | native PMGR/PCIe drivers, when needed |
| Networking on the Mac | **USB-C gadget Ethernet**, reusing `go-net/imx-usb`'s CDC-ECM logic on a new DWC3 driver; meanwhile the m1n1 proxy link | Wi-Fi (maybe never) |
| Files and storage | **GODEEPDATA** (§6.6) read by Omarchy/macOS; **9P over the USB network** from the Mac (`../ReUsingGVisor.md` §5) | native NVMe (M5) |
| Entropy | **`/chosen/random-seed`** from iBoot/m1n1 seeding the DRBG | stays (no hardware RNG reachable on M1 Pro) |
| Power off / reboot | **watchdog** reboot | SMC power-off (M3) |

Two strategies from this table matter most:
- **Lean on the firmware.** iBoot and m1n1 already initialized the display, memory, PCIe and more. Using that state instead of re-initializing it removes whole drivers from the critical path. Linux on Apple Silicon started the same way.
- **Get a network early (M4), then use the Mac as a peripheral.** Once godeep has an IP over a USB-C cable, files (9P), logs, `pprof` and even storage can come from the Mac until native drivers exist. That turns one hard driver (DWC3) into access to everything.

### 7.4 Proposed structure (small additions, not yet built)

These keep native drivers testable on the host and shareable between boards:

| Addition | Purpose |
|---|---|
| ✅ `kernel/mmio`: a `Regs` interface (`Read32`/`Write32` at an offset) with a real implementation (single atomic 32-bit accesses) and a **recording fake** that can assert or replay traces | rung 2; enforces the "no read-modify-write of W1S/W1C registers" lesson in one place. *Built.* |
| ◐ `drivers/<vendor>/<device>`: driver code as ordinary Go packages, no build tags | host tests run on every driver; boards only wire them up. *Started: `drivers/arm/{pl061,pl031}`, `drivers/virtio/mmio`. Next: `drivers/apple/{aic,dart,asc,smc,nvme,spihid,uart,wdt}`, `drivers/virtio/{rng,blk,net}`* |
| `board/applem1`: build-tagged `tamago && arm64 && applem1`; device list from the **FDT** m1n1 builds from the ADT (via `kernel/fdt`, as `board/virt` already does); registers the Apple drivers | the third board, next to `board/virt` |
| Service interfaces as needed: `dev.Block`, `dev.NIC`, `dev.Framebuffer`, `dev.Keyboard` | consumers (filesystems, netstack, shell) never depend on a specific driver |
| `make test` stays host-only; `make qemu` (✅ Image + device tree) and `make qemu EL2=1` (✅ m1n1-style entry) run the rung-3 checks; a `make m1-guest` target wraps m1n1's `run_guest.py` | one command per rung |

### 7.5 Definition of done for a native driver

A row in §2 moves from ✗/◐ to ✅ when the driver has:
1. host tests (rungs 1–2) passing 50× under the race detector;
2. a QEMU-analog run (rung 3) with `lsdev` showing it `ready`, `kill <drv pid>` followed by a clean restart, and idle CPU unchanged;
3. an m1n1-hypervisor run (rung 4) whose MMIO trace was reviewed against the Asahi/Linux driver's behavior, with the trace saved as a rung-2 test fixture;
4. a native-boot run (rung 5);
5. its §2 row, `DriverModel.md` and any new service interface updated.

---

## 8. Recommendation and milestones

**Keep QEMU `virt` + HVF as the main target.** On this Mac it already gives godeep:
- the real CPU, natively: Firestorm cores with AES, SHA3 and NEON;
- through virtio: network (Wi-Fi via QEMU's NAT), disk (an image file on the SSD), files (9P) and display (virtio-gpu in a window).

The driver foundation (§3) is developed and tested there first anyway, and §7 explains how each native driver climbs from host tests to QEMU to the real Mac.

For real-metal godeep, this order gives a visible result quickly:

| Milestone | What | Uses existing Go code | New Go code |
|---|---|---|---|
| **M0: foundation** ✅ | DMA + IOMMU interface, interrupt bridge, device manager, RTKit engine (§3) | — | **done** |
| **M0.7: boot like Linux** ✅ | `make image`, EL2 → EL1 entry, device tree from `x0` → device list, PSCI conduit from the tree, `kernel/mmio`, first portable `drivers/` (§3.1) | `kernel/fdt`, `kernel/mmio` | **done**, tested in QEMU at EL1 and EL2 |
| **M0.5: a place to boot** | the godeep stub, ESP and GODEEPDATA partition (§6); optionally the tethered-boot entry for m1n1 development | Asahi installer | none: just §6 |
| **M1: hello on the laptop screen** | `board/applem1`: MMU patch, RAM + DMA layout, boot framebuffer console (text drawn with `x/image/font`), watchdog reboot | TamaGo arm64, generic timer, **godeep's EL2 entry, Image format and FDT device list (M0.7)**, `image/draw`, `x/image/font` | MMU patch, framebuffer console, WDT |
| M2: interactive | AIC2 adapter (`irq.Controller`); keyboard via SPI-HID as a managed driver; the godeep shell on the built-in screen and keyboard | `kernel/irq`, `kernel/dev`, godeep `tty`, `sh` | AIC2 adapter, ADT parser, SPI, HID |
| M3: coprocessors | ASC mailbox (`rtkit.Mailbox`) + DART (`dma.IOMMU`); verify the RTKit encodings; SMC (battery, lid, power button) as the first client | `kernel/rtkit`, `kernel/dma` | ASC mailbox, DART, SMC protocol |
| M4: networking | DWC3 device mode + CDC-ECM, so godeep gets an IP over a USB-C cable; SSH into it | go-net/imx-usb CDC-ECM logic, `go-net`, gVisor netstack, `x/crypto/ssh` | DWC3 gadget driver |
| M5: storage | ANS2 NVMe on RTKit + SART, read-only; mount the Omarchy ext4 partition; use GODEEPDATA | `kernel/rtkit`, `kernel/dma`, a Go ext4 reader | NVMe queues, FAT32 |
| M6: everything else | SMP, Wi-Fi, audio | — | large |

- **M0.5 needs no code.** Do it whenever you're ready (§6).
- **M1** is a weekend-sized experiment once you have a boot path. It's worth doing just to see the gopher-made shell on the XDR display.
- **M3** is where the foundation pays off: every RTKit device after it is "a protocol on an endpoint channel".
- **M4** is where real hardware becomes as useful as QEMU, and **M6** is where it stops being worth it.

---

## References

- Asahi Linux docs: the hardware feature table for `t6000`/J316s; the m1n1 user guide (proxy, chainloading, hypervisor); the installer and its "UEFI environment only" / "Tethered boot" options; partitioning and removing an install; device-tree bindings
- m1n1 source (`aic.c`, `uart.c`, `smp.c`, `rtkit.c`, `dart.c`, `proxyclient/`)
- Linux / Asahi kernel drivers named in the tables above (`drivers/irqchip/irq-apple-aic.c`, `drivers/mailbox/apple-mailbox.c`, `drivers/soc/apple/rtkit*.c`, `drivers/nvme/host/apple.c`, `drivers/iommu/apple-dart.c`, `drivers/pci/controller/pcie-apple.c`, …)
- TamaGo: `arm64` (boot, MMU, timers), `arm64/gic`, `soc/nxp/usdhc`; usbarmory `go-net` (incl. `go-net/imx-usb`)
- `diskutil apfs resizeContainer`, `diskutil addPartition`, `diskutil apfs deleteContainer` (`diskutil <verb>` with no arguments prints usage)
- godeep: [`DriverModel.md`](DriverModel.md) (the §3 foundation), `board/virt` (the QEMU arm64 board an M1 board would sit next to), `../GoGpuSupport.md`, `../ReUsingGVisor.md`
