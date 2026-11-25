# Targeting my Mac: hardware inventory and Go driver coverage

This doc does three things:
- lists the hardware in the actual machine godeep is developed on;
- checks, for each part, whether **an existing Go driver** can talk to it;
- lays out what it would take to run godeep on this Mac's real hardware instead of in QEMU.

**How the inventory was gathered (2026-11-25).** Everything below came from the running machine through system tools; no web lookups were needed:
- `system_profiler` (hardware, displays, memory, NVMe, storage, Wi-Fi, Bluetooth, Thunderbolt, card reader, audio, camera, power, SPI)
- `sysctl` (CPU topology, caches, `hw.optional.arm.FEAT_*`)
- `diskutil list`
- `ioreg -p IODeviceTree`: Apple's device tree. Its `compatible` strings are the names Linux and Asahi drivers bind to, so they're the key for finding driver code.

The serial number is deliberately left out of this file, since the repo may be shared.

---

## TL;DR

- **The machine:** a 14-inch MacBook Pro (2021), `MacBookPro18,1`, board `J316sAP`, **Apple M1 Pro** (SoC `t6000`). 8 performance + 2 efficiency cores, 16-core GPU, 16 GB LPDDR5, 512 GB SSD.
- **Go driver coverage on bare metal is thin.** What works out of the box is the *CPU* side: TamaGo's arm64 boot, MMU and timers, plus Go's own assembly-accelerated crypto and SIMD. **Almost every peripheral has no Go driver at all.** The reference code is Asahi Linux (C/Rust) and its bootloader m1n1 (C/Python), and each driver would have to be ported to Go.
- **Through QEMU (today's setup)**, godeep already uses this Mac's CPU natively via HVF, and reaches everything else through virtio: network, disk, display and files. That remains the practical target.
- **For bare metal, this Mac is unusually well placed.** It already has an **Asahi-based Linux install ("Omarchy")**, so Asahi's **m1n1** bootloader is set up. m1n1 can load a non-Linux kernel, and its hypervisor mode can run one while logging every hardware access. The first milestone would be "godeep prints its shell on the laptop screen" using the framebuffer the firmware already set up, which needs **no display driver at all**.

---

## 1. Hardware inventory

### Machine

| Item | Value |
|---|---|
| Model | MacBook Pro, `MacBookPro18,1` (model number MK183LL/A) |
| Board ID (device tree) | `J316sAP` |
| OS | macOS 26 (Darwin 25.6) + Omarchy Linux (Asahi-based) on the same SSD |
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
| Partitions | macOS APFS (357 GB), **Omarchy APFS stub (2.5 GB), `EFI - OMARC` (524 MB), 2 Linux filesystems (2.1 + 132 GB)**, Recovery | `diskutil list` |
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

## 2. Which existing Go drivers can talk to it

Legend:
- ✅ works as-is
- ◐ partial: Go code exists for part of the job and can be adapted
- ✗ no Go driver exists; port from the reference code

Effort: XS = hours, S = days, M = 1–3 weeks, L = 1–3 months, XL = a project of its own. "Reference" is what you'd port from.

### CPU-level (where Go is strong)

| Hardware | Existing Go code | Status | Notes / reference | Effort |
|---|---|---|---|---|
| CPU boot, exceptions, caches | `tamago/arm64` | ◐ | Needs two patches (§3): an EL2 entry path and an MMU that reaches RAM at `0x100_0000_0000` | S |
| MMU / page tables | `tamago/arm64` mmu | ◐ | Hard-codes a 39-bit VA with flat mapping. Needs a 48-bit VA (T0SZ=16), 4-level tables and 16 KB-aware DMA mappings | S–M |
| Generic timer | `tamago/arm64` `InitGenericTimers`, `GetTime` | ✅ | Architected and identical to QEMU. The timer *interrupt* is delivered as FIQ through AIC, not GIC | — |
| AES, SHA-1/2/3, GHASH | Go std `crypto/*` (arm64 assembly) | ✅ | Uses the CPU's AES, SHA3 and PMULL instructions directly | — |
| NEON SIMD | Go `simd`, `simd/archsimd` (GOEXPERIMENT=simd) | ✅ | Already enabled in the Makefile | — |
| Atomics | Go `sync/atomic` (LSE) | ✅ | | — |
| Random numbers | `board/virt` software DRBG | ◐ | No FEAT_RNG on M1 Pro, and the SEP TRNG isn't accessible. Seed from the firmware's `/chosen/random-seed` (iBoot provides it at boot; m1n1 passes it on) | XS |
| More cores (SMP) | none (TamaGo has no arm64 SMP) | ✗ | Apple starts cores through PMGR "cpu-start" registers, not PSCI. Reference: m1n1 `smp.c`, Linux `arch/arm64` + Apple PMGR | M |
| Interrupt controller (AIC2) | `tamago/arm64/gic` doesn't apply | ✗ | Small driver. Reference: Linux `irq-apple-aic.c`, m1n1 `aic.c` | S |
| Power off / reboot | `board/virt` PSCI | ✗ | No PSCI firmware on Apple Silicon. Reboot through the watchdog (Linux `apple_wdt.c`, tiny); power-off through SMC | XS / M |

### Peripherals

| Hardware | Existing Go driver? | Status | Reference to port | Effort |
|---|---|---|---|---|
| **Boot framebuffer** (screen, as set up by iBoot/m1n1) | none needed: plain memory + Go `image/draw`, `golang.org/x/image/font` | ✅ | Address/size/stride come from m1n1's boot args (the same thing Linux's `simplefb` uses) | XS–S |
| Debug UART (Samsung) | none | ✗ | Linux `samsung_tty.c`, m1n1 `uart.c`; ~100 lines. Needs Apple debug-USB tooling or a second Apple Silicon Mac (`macvdmtool`) to reach | XS |
| Watchdog | none | ✗ | Linux `apple_wdt.c` | XS |
| GPIO / pin control | none | ✗ | Linux `pinctrl-apple-gpio.c` | S |
| I²C | none on bare metal (`periph.io` drivers need a host OS) | ✗ | Linux `i2c-pasemi-*.c` | S |
| PMGR power gates | none | ✗ | Linux `apple-pmgr-pwrstate.c` | S |
| DART IOMMU | none | ✗ | Linux `apple-dart.c`, m1n1 `dart.c`. Needed by every DMA device | S–M |
| RTKit mailbox (coprocessors) | none | ✗ | Linux `drivers/soc/apple/rtkit*.c`, m1n1 `rtkit.c`. Gatekeeper for NVMe, SMC, display, audio | M |
| SMC (battery, sensors, lid, power button) | none | ✗ | Asahi `macsmc` (on top of RTKit) | M |
| **Keyboard / trackpad** (HID over SPI) | none (Go HID libraries wrap host hidapi) | ✗ | Linux `spi-apple.c` + Asahi's SPI-HID transport; HID report parsing | M |
| **SSD** (ANS2) | none (no Go NVMe driver, and this one isn't standard PCIe NVMe) | ✗ | Linux `drivers/nvme/host/apple.c` (on top of RTKit + SART) | M–L |
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
| Display controller (DCP) | none | ✗ | Asahi DCP driver (RTKit-based). Use the boot framebuffer instead | XL |
| External displays / HDMI | none | ✗ | DCP + Parade DP855 + Type-C alt modes | XL |
| GPU (AGX) | none | ✗ | Asahi's Rust `drm/asahi` + Mesa. See `GoGpuSupport.md` | XL+ |
| Neural Engine | none | ✗ | Reverse-engineered, unofficial | XL+ |
| Video encode/decode, JPEG, scaler | none | ✗ | Partial Asahi work | L–XL |
| Speakers, headphones, mics | none | ✗ | Asahi `macaudio`, `mca`, `admac`, TAS2764, CS42L84 drivers | L |
| Camera (ISP) | none | ✗ | Asahi ISP driver, firmware-dependent | XL |
| Touch ID / Secure Enclave | none | ✗ | Closed SEP firmware; not feasible | — |
| AES engine / SEP crypto | none | ✗ | Not needed: the CPU's crypto instructions (✅) are faster for godeep | — |
| Boot NOR flash / NVRAM | none | ✗ | **Don't touch**: writing here can make the Mac unbootable | — |
| Battery / charging info | none | ✗ | Comes free once SMC works | S (after SMC) |
| Keyboard backlight (PWM) | none | ✗ | Linux `pwm-apple.c`; trivial | XS |

**Scorecard:**
- Of roughly 40 hardware blocks, **5 work with existing Go code** (timer, crypto, SIMD, atomics, the boot framebuffer through Go's `image` packages) plus the gVisor network stack once something supplies frames.
- **5 have partial Go code to adapt:** TamaGo arm64 boot/MMU, the RNG, ext4 readers, uSDHC for SD, and go-net/imx-usb for USB Ethernet.
- **Everything else is a port.** The good news: Asahi made all of it open source and documented, and it's still the only complete driver reference for this hardware.

---

## 3. TamaGo blockers specific to this Mac

These must be fixed before godeep can print a single character on the real hardware:

1. **Entry at EL2.** m1n1 starts payloads at **EL2** (Asahi kernels run in EL2/VHE). `tamago/arm64` `cpuinit` handles EL1 or EL3 only: from EL2 it would execute EL3 register writes, which is an undefined-instruction fault. Add an EL2→EL1 drop (set `HCR_EL2.RW`, then `ERET` to EL1). That's about 15 lines of assembly in a board-specific `linkcpuinit` override.
2. **RAM is above 1 TiB.** DRAM starts at `0x100_0000_0000`. TamaGo's arm64 MMU sets up a 39-bit VA with a 40-bit physical range and flat mapping, which can't reach it. Either patch the MMU (48-bit VA, 4-level tables) or map RAM at a low virtual address. Flat mapping plus a 48-bit VA is the simpler patch.
3. **AIC, not GIC.** The idle routine in `board/virt` uses the GIC. On real hardware, use the timer FIQ through AIC, or simply poll-WFI with the timer.
4. **No PSCI.** `halt` must use the watchdog or SMC instead.
5. **Image format.** m1n1 and U-Boot load raw arm64 "Image" binaries (with Linux's 64-byte header), not ELF. Add a `make image` step: `objcopy -O binary` plus a header shim.
6. **16 KB IOMMU pages.** DART works in 16 KB pages, so DMA buffers must be 16 KB-aligned even if the CPU uses 4 KB pages.

---

## 4. How godeep would boot on this Mac

Thanks to the Omarchy (Asahi-based) install, Asahi's boot chain already exists:

```
iBoot (Apple)  →  m1n1 stage 1 (in the "Omarchy" APFS stub)
               →  m1n1 stage 2 + device tree (on "EFI - OMARC")  →  U-Boot  →  GRUB/Linux
```

Ways to slot godeep in, safest first:

| Option | How | Needs | Risk |
|---|---|---|---|
| **A. m1n1 proxy + hypervisor** (dev loop) | Reboot into m1n1 proxy mode and push godeep over USB-C with m1n1's `proxyclient` tools (`chainload.py` / `run_guest.py`). In hypervisor mode, m1n1 runs godeep as a guest and **logs every hardware access**, which is the ideal way to debug new drivers | A **second computer** connected over USB-C | Nothing is written to disk |
| B. U-Boot boot entry | Copy `godeep.img` to the `EFI - OMARC` partition and boot it from the U-Boot prompt (`booti`) | Only this Mac | Low: macOS and Omarchy stay untouched, but you're editing the Linux boot partition, so back it up first |
| C. Separate stub install | Run the Asahi installer again and choose the "UEFI environment only / m1n1" option to create a separate boot entry just for godeep | Only this Mac | Low; uses a little more disk |

Never write to the macOS container, the APFS stub internals, NOR flash or the Recovery partition. The macOS Startup Disk picker (hold the power button at boot) always gets you back.

---

## 5. Recommendation and milestones

**Keep QEMU `virt` + HVF as the main target.** On this Mac it already gives godeep:
- the real CPU, natively: Firestorm cores with AES, SHA3 and NEON;
- through virtio: network (Wi-Fi via QEMU's NAT), disk (an image file on the SSD), files (9P) and display (virtio-gpu in a window).

That's every *useful* capability of this hardware with none of the driver work.

If you want real-metal godeep anyway, this order gives a visible result quickly:

| Milestone | What | Uses existing Go code | New Go code |
|---|---|---|---|
| **M1: hello on the laptop screen** | `board/applem1`: EL2 entry, MMU patch, RAM layout, boot framebuffer console (text drawn with `x/image/font`), watchdog reboot | TamaGo arm64, generic timer, `image/draw`, `x/image/font` | EL2 shim, MMU patch, framebuffer console, WDT |
| M2: interactive | AIC; keyboard via SPI-HID; the godeep shell on the built-in screen and keyboard | godeep `tty`, `sh` | AIC, SPI, HID |
| M3: networking | DWC3 device mode + CDC-ECM, so godeep gets an IP over a USB-C cable to another computer; SSH into it | go-net/imx-usb CDC-ECM logic, `go-net`, gVisor netstack, `x/crypto/ssh` | DWC3 gadget driver |
| M4: storage | RTKit + SART + ANS2 NVMe, read-only; mount the Omarchy ext4 partition | a Go ext4 reader | RTKit, NVMe |
| M5: everything else | SMP, SMC (battery, lid), Wi-Fi, audio | — | large |

M1 is a weekend-sized experiment once you have option A or B. It's worth doing just to see the gopher-made shell on the XDR display. M3 is where the real-hardware path becomes as useful as QEMU, and M5 is where it stops being worth it.

---

## References

- Asahi Linux docs: the hardware feature table for `t6000`/J316s, m1n1 user guide (proxy, chainloading, hypervisor), device-tree bindings
- m1n1 source (`aic.c`, `uart.c`, `smp.c`, `rtkit.c`, `dart.c`, `proxyclient/`)
- Linux / Asahi kernel drivers named in the tables above (`drivers/irqchip/irq-apple-aic.c`, `drivers/nvme/host/apple.c`, `drivers/soc/apple/`, `drivers/iommu/apple-dart.c`, `drivers/pci/controller/pcie-apple.c`, …)
- TamaGo: `arm64` (boot, MMU, timers), `soc/nxp/usdhc`; usbarmory `go-net` (incl. `go-net/imx-usb`)
- godeep: `board/virt` (the QEMU arm64 board this would sit next to), `GoGpuSupport.md`, `ReUsingGVisor.md`
