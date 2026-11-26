# Targeting my Mac: hardware, Go drivers, and a place to boot godeep

This doc covers everything about running godeep on the actual machine it's developed on, instead of in QEMU: the hardware, which Go drivers can talk to it, godeep's driver foundation, the remaining blockers, how godeep boots here, a guide to carving out a godeep partition, how to develop native drivers incrementally, and the milestone order.

## Contents

- [TL;DR](#tldr)
- [1. Hardware inventory](#1-hardware-inventory)
  - [Machine](#machine)
  - [SoC, CPU, memory](#soc-cpu-memory)
  - [Interrupts, timers, power and housekeeping](#interrupts-timers-power-and-housekeeping)
  - [Storage](#storage)
  - [Display, GPU, media](#display-gpu-media)
  - [Networking and wireless](#networking-and-wireless)
  - [USB, Thunderbolt, PCIe](#usb-thunderbolt-pcie)
  - [Input, audio, camera, biometrics, power](#input-audio-camera-biometrics-power)
- [2. Which Go code can talk to it](#2-which-go-code-can-talk-to-it)
  - [CPU-level (where Go is strong)](#cpu-level-where-go-is-strong)
  - [Peripherals](#peripherals)
- [3. godeep's driver foundation (built, 2026-11-25)](#3-godeeps-driver-foundation-built-2026-11-25)
  - [3.1 Booting like a Linux kernel (built, 2026-11-25)](#31-booting-like-a-linux-kernel-built-2026-11-25)
  - [3.2 Decision: how godeep handles an EL2 entry (2026-11-25)](#32-decision-how-godeep-handles-an-el2-entry-2026-11-25)
  - [3.3 After the first native boot: keyboard, idle, watchdog, storage (built 2026-11-25, keyboard and trackpad confirmed on the Mac 2026-11-26)](#33-after-the-first-native-boot-keyboard-idle-watchdog-storage-built-2026-11-25-keyboard-and-trackpad-confirmed-on-the-mac-2026-11-26)
  - [3.4 The QEMU build-out, and what carries to the Mac (2026-11-26)](#34-the-qemu-build-out-and-what-carries-to-the-mac-2026-11-26)
  - [3.5 Drivers the Mac's device tree asks for (from `lsdev`, 2026-11-25/26)](#35-drivers-the-macs-device-tree-asks-for-from-lsdev-2026-11-2526)
- [4. TamaGo blockers specific to this Mac](#4-tamago-blockers-specific-to-this-mac)
- [5. How godeep would boot on this Mac](#5-how-godeep-would-boot-on-this-mac)
- [6. Carving out a godeep partition next to Macintosh HD and Omarchy](#6-carving-out-a-godeep-partition-next-to-macintosh-hd-and-omarchy)
  - [6.1 What you'll end up with](#61-what-youll-end-up-with)
  - [6.2 Safety first](#62-safety-first)
  - [6.3 Pre-flight checks](#63-pre-flight-checks)
  - [6.4 Step 1: shrink Macintosh HD](#64-step-1-shrink-macintosh-hd)
  - [6.5 Step 2: create the bootable godeep stub (Asahi installer)](#65-step-2-create-the-bootable-godeep-stub-asahi-installer)
  - [6.6 Step 3 (optional): the GODEEPDATA partition](#66-step-3-optional-the-godeepdata-partition)
  - [6.7 Step 4: verify](#67-step-4-verify)
  - [6.8 Step 5: put godeep on its ESP and boot it](#68-step-5-put-godeep-on-its-esp-and-boot-it)
  - [6.9 Undo: remove godeep and give the space back](#69-undo-remove-godeep-and-give-the-space-back)
  - [6.10 Troubleshooting](#610-troubleshooting)
- [7. Developing native drivers incrementally](#7-developing-native-drivers-incrementally)
  - [7.1 The principle: one kernel, swappable boards, drivers behind interfaces](#71-the-principle-one-kernel-swappable-boards-drivers-behind-interfaces)
  - [7.2 The development ladder for each driver](#72-the-development-ladder-for-each-driver)
  - [7.3 Borrow what isn't native yet](#73-borrow-what-isnt-native-yet)
  - [7.4 Proposed structure (small additions, not yet built)](#74-proposed-structure-small-additions-not-yet-built)
  - [7.5 Definition of done for a native driver](#75-definition-of-done-for-a-native-driver)
  - [7.6 What's still worth building on QEMU, and what needs two machines](#76-whats-still-worth-building-on-qemu-and-what-needs-two-machines)
- [8. Recommendation and milestones](#8-recommendation-and-milestones)
- [References](#references)

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
- **Status (2026-11-26): godeep is interactive on this Mac.** First boot 2026-11-25 (m1n1 → godeep, shell output on the built-in display, `screens/IMG_2341.JPG`). Second boot 2026-11-26 (`screens/IMG_2342.JPG`, §3.3): the **internal keyboard** types into the shell (SPI-HID, polled, so no interrupt controller needed), the **trackpad** is in multitouch mode, **WFE idle** sleeps instead of spinning, and the **watchdog** is armed and petted. `make native` tests, rehearses on QEMU and installs. On QEMU, godeep now reads disks: **PCIe → NVMe → GPT → FAT32/ext4**, the stack the Mac's SSD will use once its Apple-specific layer exists.
- **Since then, on QEMU (2026-11-26, §3.4):** networking (virtio-net, gVisor TCP/IP, DHCP, DNS), SSH with SFTP/scp, WASM programs that run without a reboot, a RAM filesystem and a host share (9P), USB hubs and USB sticks, a kernel log and profiles, and a framebuffer console that responds to the mouse. All of it is in the Mac image too; most of it waits for **one Mac driver**: a network link (USB-C gadget Ethernet, M4) or the debug UART under m1n1's hypervisor.
- **Drivers the Mac's own device tree asks for** (§3.5): the `lsdev` photos list what m1n1 describes that godeep doesn't drive yet. The nearest: the **Samsung UART** (`serial@39b200000`), the **SMC** (`smc@290400000`), **DWC3** USB (three controllers) with their **CD321x** port controllers, **SPMI**, and the keyboard-backlight **PWM**.
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
| Random numbers | software DRBG in each board (`board/*/rng.go`) | ◐ | QEMU: seeded by **virtio-rng** (built, §3.4). The Mac still seeds from the boot counter (**not secure**, and sshd's session keys depend on it). No FEAT_RNG on M1 Pro, and the SEP TRNG isn't accessible: mix in the seed iBoot/m1n1 put in `/chosen` (`rng-seed` / `kaslr-seed`; check the tree) | XS |
| More cores (SMP) | none (TamaGo has no arm64 SMP) | ✗ | Apple starts cores through PMGR "cpu-start" registers, not PSCI. Reference: m1n1 `smp.c`, Linux `arch/arm64` + Apple PMGR | M |
| **Interrupt controller (AIC2)** | **godeep `kernel/irq` bridge** (routing, oneshot masking, stats, latency probe) | ◐ | Only the controller adapter is left: implement `irq.Controller` (`Enable`/`Disable`) plus the ack loop for AIC2's event register. Reference: Linux `irq-apple-aic.c`, m1n1 `aic.c` | S |
| Power off / reboot | `drivers/apple/wdt` (reboot) | ◐ | No PSCI firmware on Apple Silicon. **Reboot works** through the watchdog. Power-off needs SMC; the kernel's shutdown path (`Board.PowerOff`) is ready | M (SMC) |
| **Device discovery + driver supervision** | **godeep `kernel/dev`** device manager + **`kernel/fdt`** parser | ◐ | Drivers run as supervised, restartable processes, and the device list comes from the FDT in `x0` (§3.1). m1n1 converts Apple's ADT into an FDT for its payload, so no ADT parser is needed. Left: check m1n1's FDT for this Mac, and map AIC2's interrupt cells | XS |

### Peripherals

| Hardware | Go code | Status | Reference to port | Effort |
|---|---|---|---|---|
| **Boot framebuffer** (screen, as set up by iBoot/m1n1) | `drivers/simplefb` | ✅ | **Built and running on the Mac.** Text console with 2000 lines of scrollback; the trackpad moves a pointer, two-finger scroll goes back through history, click-drag selects (`simplefb.Mouse`, fed by `inputd`, §3.4) | — |
| **Debug UART (Samsung)** | none | ✗ | **Next driver** (`serial@39b200000`, `apple,s5l-uart`, in the tree). Linux `samsung_tty.c`, m1n1 `uart.c`; ~100 lines. Natively it needs Apple debug-USB tooling or a second Apple Silicon Mac (`macvdmtool`); **under m1n1's hypervisor it's forwarded to the second computer as a USB serial port** (`Using2Computers.md` §6), which gives text console and even SSH (`SSHServer.md` §2.3) | XS |
| Watchdog | `drivers/apple/wdt` | ◐ | Built from Linux `apple_wdt.c`; **armed and petted on the Mac** (2026-11-26), `reboot`. Left: confirm it stays up past 30 s | XS |
| GPIO / pin control | `drivers/apple/gpio` | ◐ | Outputs, input levels, pin mux (Linux `pinctrl-apple-gpio.c`, U-Boot `pinctrl-apple.c`); no interrupts yet | S |
| I²C | none on bare metal (`periph.io` drivers need a host OS) | ✗ | Linux `i2c-pasemi-*.c`. The **CD321x** USB-C port controllers hang off it (four in the tree, `usb-pd@38/3a/3b/3f`) | S |
| **SPMI** (PMIC bus) | none | ✗ | `spmi@2920a1300` (`apple,t6000-spmi`) in the tree. Linux `spmi-apple-controller.c`; the PMIC behind it holds boot flags (NVMEM) | S |
| PMGR power gates | `drivers/apple/pmgr` | ◐ | Turning domains on, parents first, from `power-domains` (Linux `apple-pmgr-pwrstate.c`) | S |
| **DART IOMMU** | **godeep `kernel/dma`**: allocator outside the Go heap (16 KB alignment, ownership, stats) + the `dma.IOMMU` interface drivers already call | ◐ | Only the DART page tables and registers are left: implement `Map`/`Unmap`. Reference: Linux `apple-dart.c`, m1n1 `dart.c` | S–M |
| **RTKit coprocessors** | **godeep `kernel/rtkit`**: handshake, endpoint discovery, power states, shared-buffer requests (via `kernel/dma`), crash detection, per-endpoint channels | ◐ | Left: the ASC mailbox hardware driver (implements `rtkit.Mailbox`), and checking the message encodings on real hardware. Reference: Linux `apple-mailbox.c`, `drivers/soc/apple/rtkit*.c`, m1n1 `rtkit.c` | S |
| SMC (battery, sensors, lid, power button) | rides on `kernel/rtkit` | ✗ | The SMC protocol on top of an RTKit endpoint. Asahi `macsmc` | S–M |
| **Keyboard / trackpad** (HID over SPI) | `drivers/apple/spi` + `drivers/apple/spihid` + `kernel/input` | ✅ | **Working on the Mac (2026-11-26)**, polled like U-Boot's `apple_spi_kbd.c` (no AIC needed): reads the ready line (nub GPIO 6), 121/121 packets CRC-ok. Keyboard: US layout, Ctrl, Caps Lock, repeat. Trackpad (after Asahi's `hid-magicmouse.c`): multitouch on, one finger moves the console pointer, two scroll, click. Left: interrupts (AIC2), other layouts | — |
| **SSD** (ANS2) | `drivers/nvme` (command layer) + `kernel/rtkit` + `kernel/dma` | ◐ | NVMe queues, identify, reads and PRP lists are built and run on QEMU virt (`make check-virt`). Left: ANS2's differences (linear submission, NVMMU/TCBs), SART, RTKit boot. Linux `drivers/nvme/host/apple.c` | M |
| Filesystems on the SSD | `kernel/block` (GPT), `fs/ext4`, `fs/fat`, `kernel/vfs` | ◐ | Built, read-only, tested on real mkfs images and on QEMU, on NVMe and USB sticks. `kernel/vfs` names volumes (`nvme0p2:`, `usbdisk0p1:`, `ram:`, `host:`). Waits for the SSD driver to read the Omarchy partition; never write to APFS. The SSD also needs **SART** (`sart@393c50000` in the tree) | M |
| PCIe root complex | `tamago/soc/intel/pci` is x86 port I/O, doesn't apply | ✗ | Linux `pcie-apple.c`. m1n1 can bring the links up before handing over | M |
| SD reader (GL9755, SDHCI) | `tamago/soc/nxp/usdhc` (different controller, same SD protocol) | ◐ | Linux `sdhci-pci-gli.c`; reuse the SD command logic from usdhc | M |
| **USB-C in device mode** (gadget) | CDC-ECM framing: `usbarmory/go-net/imx-usb` (written for NXP's controller) as reference | ◐ | Needs a Go DWC3 *device-mode* driver (Linux `dwc3`) that publishes a `netdev.NIC`. Everything above it is **built** (§3.4): `netd` runs the TCP/IP stack on the first NIC it finds, then DHCP, sshd, SFTP, WASM loading, 9P all work over the cable | M–L |
| USB host (DWC3 xHCI) | `drivers/usb/xhci` + `drivers/usb` | ◐ | The xHCI host driver runs on QEMU (`make check-virt`): HID boot keyboards and mice, **hubs**, **USB sticks** (Bulk-Only + SCSI, read-only, published as `usbdisk<N>`). Three DWC3 controllers in the tree (`usb@702280000`, `b02280000`, `f02280000`). Left for the Mac: DWC3 host-mode setup, the Type-C PHY (ATC), the CD321x port controllers (I²C), and **transaction translators**: a keyboard or mouse behind a *high-speed* hub (any USB-C dock) needs the hub's TT in its slot context, which the driver doesn't set yet | L |
| USB-C PD controller (CD3217) | none | ✗ | Linux `tipd` driver; needed to switch port roles and modes | M |
| Thunderbolt / USB4 | none | ✗ | Asahi work in progress | XL |
| **Wi-Fi** (BCM4387) | none | ✗ | Linux `brcmfmac` plus the firmware blobs the Asahi installer copies from macOS | XL |
| Bluetooth (BCM4387) | none | ✗ | Linux `hci_bcm4377.c` | L |
| Network stack (TCP/IP) | **`net/netstack`**: gVisor's TCP/IP with godeep's own glue | ✅ | Built and running on QEMU (§3.4): any `netdev.NIC`, DHCP, DNS, `cap.Net` listeners and dials; sshd + SFTP on top. On the Mac it waits for a NIC (USB-C gadget Ethernet) | — |
| Display controller (DCP) | none (would ride on `kernel/rtkit`) | ✗ | Asahi DCP driver. Use the boot framebuffer instead | XL |
| External displays / HDMI | none | ✗ | DCP + Parade DP855 + Type-C alt modes | XL |
| GPU (AGX) | none | ✗ | Asahi's Rust `drm/asahi` + Mesa. See `GoGpuSupport.md` | XL+ |
| Neural Engine | none | ✗ | Reverse-engineered, unofficial | XL+ |
| Video encode/decode, JPEG, scaler | none | ✗ | Partial Asahi work | L–XL |
| Speakers, headphones, mics | none (audio coprocessor would ride on `kernel/rtkit`) | ✗ | Asahi `macaudio`, `mca`, `admac`, TAS2764, CS42L84 drivers | L |
| Camera (ISP) | none | ✗ | Asahi ISP driver, firmware-dependent | XL |
| Touch ID / Secure Enclave | none | ✗ | Closed SEP firmware; not feasible | — |
| AES engine / SEP crypto | none | ✗ | Not needed: the CPU's crypto instructions (✅) are faster for godeep | — |
| Boot NOR flash / NVRAM | none | ✗ | **Don't touch**: writing here can make the Mac unbootable | — |
| Battery / charging info | none | ✗ | Comes free once SMC works | S (after SMC) |
| Keyboard backlight (PWM) | none | ✗ | `apple,t6000-fpwm` in the tree. Linux `pwm-apple.c`; trivial | XS |
| SIO coprocessor | rides on `kernel/rtkit` | ✗ | `sio@39bc00000` (`apple,t6000-sio`): RTKit-based DMA for audio and DisplayPort. Asahi `sio` | M |

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
| Device manager | `kernel/dev` | each driver is a process `drv/<name>` supervised by `devmgr`: dependency ordering (`Needs`), readiness (`Ready`/`Get`), restart with backoff when any driver goroutine panics | PL061 power button, PL031 RTC, virtio-mmio scanner, timer probe; PCIe host + NVMe added by the PCIe driver at run time (`lsdev`) | **ADT parser** producing `dev.Node`s from m1n1's device tree |
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

Added for the M1 (2026-11-25), all tested in QEMU except where noted:

| Piece | Where | What it does |
|---|---|---|
| **self-relocation** | top of `cpuinit` in both boards' `entry_arm64.s` | m1n1 doesn't honour `text_offset`: it copies the kernel to *any* 2 MB-aligned block of its heap (`payload.c`) and jumps to it with the FDT in `x0` (`kboot.c`). U-Boot's `booti` puts it at `ALIGN(usable RAM start, 2 MB) + text_offset`, and usable RAM starts at iBoot's `phys_base`, which varies. So `cpuinit` checks where it's running (PC-relative), copies the image to its link address if different (either direction, overlap-safe), invalidates the I-cache and restarts. QEMU tests: loaded 2 MB above, 60 KB below and exactly at the link address |
| **4-level, 48-bit page tables** | `kernel/pgtable` (host-tested, allocation-free) + each board's `mmu.go` (`Hwinit0`) | reaches RAM at 1 TB; 1 GB/2 MB blocks, 4 KB pages only at region edges; Device, Normal, Normal-exec and Normal non-cacheable (framebuffer) types. Replacing TamaGo's `Hwinit0` needed a two-file patch to the vendored TamaGo (`third_party/tamago/GODEEP_PATCHES.md`). `board/virt` runs it daily |
| **early identity map** | `cpuinit` | Go code can't run with the MMU off (every access is Device memory, and Go's vector loads/stores take alignment faults), so `cpuinit` maps RAM with 1 GB blocks in assembly before the runtime starts, as Linux does |
| **VHE-aware EL2 drop** | `cpuinit` | Apple cores force `HCR_EL2.E2H = 1`, which changes the `CPTR_EL2` and `CNTHCTL_EL2` layouts. `cpuinit` reads E2H back and programs the right one, so FP/SIMD and the EL1 timer aren't left trapped. Also writes `VTTBR_EL2 = 0` (§3.2) and a fully known `SCTLR_EL1`. *E2H = 1 can only be tested on the Mac* |
| **framebuffer console** | `drivers/simplefb` (host-tested, renders to PNG) | text on m1n1's `simple-framebuffer`: `x8r8g8b8`, `x2r10g10b10` (the XDR panel), `r5g6b5`; scaled font, scrolling |
| **`board/applem1`** | `make image BOARD=applem1` → `bin/godeep-applem1.img` | the Mac board: memory map below, framebuffer console (and runtime panics on screen), device list from m1n1's FDT, check that godeep's RAM is inside m1n1's usable memory and clear of `/reserved-memory`, **autorun** of `lsdev`, `ps`, `mem`, `uptime`, then `sleep 5` and `lsdev idle watchdog hid` (the keyboard row says how far bring-up got), the internal keyboard (`drivers/apple/spihid`, polled), WFE idle woken by the timer event stream (`board/applem1/idle.go`; the `idle` row in lsdev gives the wake-up rate: ~1,500/s means WFE sleeps, millions means it spins), the watchdog (`drivers/apple/wdt`: armed with 30 s, petted every 10 s, used by `reboot`, stopped on `halt` and on fault screens so they stay readable), boot-stage squares and a fault screen before the console (§6.8). *Boots to the shell on QEMU sbsa-ref (`make check-applem1`) and on this Mac (2026-11-25, via m1n1, `screens/IMG_2341.JPG`)* |

`board/applem1` memory map (16 GB model): DRAM `0x100_0000_0000`–`0x104_0000_0000`; godeep RAM `0x101_0000_0000` + 4 GB (kernel linked at `+1 MB`, page tables below it); DMA region `0x102_0000_0000` + 512 MB (first 2 MB hold the boot device tree); everything else in DRAM mapped non-cacheable (firmware, the framebuffer).

The first boot line says how godeep was entered, for example:

```
board: qemu-virt; entered at EL2, dropped to EL1; linux,dummy-virt, device tree from boot loader (10 devices); PSCI via smc
```

### 3.2 Decision: how godeep handles an EL2 entry (2026-11-25)

**Decision: Option A, drop from EL2 to EL1** in `board/applem1`'s early assembly, before Go's runtime or the MMU starts. After the `eret`, TamaGo's stock EL1 runtime and MMU code runs unchanged. This is the simplest path for TamaGo and the same one `board/virt` already uses and tests.

The sequence:
1. Read `CurrentEL`. If it's EL2 (raw register value `0x8`, i.e. EL field = 2), continue; if EL1, skip to the Go entry.
2. `HCR_EL2`: set bit 31 (`RW = 1`), so EL1 runs in 64-bit AArch64 mode.
3. Turn EL2's stage-2 translation off: `VTTBR_EL2 = 0` (and `HCR_EL2.VM = 0`).
4. `SPSR_EL2 = 0x3c5`: return to EL1h with D, A, I and F masked.
5. `ELR_EL2 =` the Go entry address.
6. `eret`. From the next instruction on, the CPU is at EL1 and TamaGo's code applies as-is.

How that maps to today's implementation (`board/virt/entry_arm64.s`, tested with `make qemu EL2=1`):

| Step | In `entry_arm64.s` today | Note |
|---|---|---|
| Check `CurrentEL` | yes: shifts the EL field out and compares with 2 (same test as raw `0x8`) | |
| `HCR_EL2.RW = 1` | yes: writes `HCR_EL2 = 1<<31` | writing only bit 31 also clears `VM`, `IMO`/`FMO` (interrupts go to EL1) and all trap bits |
| Stage 2 off | via `HCR_EL2.VM = 0` (the write above) | **`VTTBR_EL2 = 0` is not written yet.** Add it (one `msr`) so the decision is followed literally: belt and braces against firmware leaving a stale table pointer |
| `SPSR_EL2 = 0x3c5` | yes | |
| `ELR_EL2` = entry | yes: `·el1entry`, which captures the device tree and then starts TamaGo's runtime | |
| `eret` | yes | |
| extra setup it also does | `CNTHCTL_EL2 \|= EL1PCTEN\|EL1PCEN` (EL1 can use the physical timer and counter), `CNTVOFF_EL2 = 0`, `HSTR_EL2 = 0`, `CPTR_EL2 = 0x33ff` (no FP/SIMD traps), `ICC_SRE_EL2 = SRE\|Enable` (EL1 may use the GIC system registers) | needed on QEMU; on the M1 the GIC line doesn't apply (AIC2 is MMIO), and the timer and FP lines still matter |

**Why not stay at EL2?** Running the kernel at EL2 (as Asahi's Linux does with VHE, `HCR_EL2.E2H`) would mean porting TamaGo's exception, MMU and timer code to EL2 registers, all for features godeep doesn't need: it hosts no VMs. Dropping to EL1 keeps godeep on TamaGo's tested path. If godeep ever wants to be a hypervisor, the EL2 vectors can be added then.

**Action items:**
- add `VTTBR_EL2 = 0` to the EL2 path in `board/virt/entry_arm64.s`, and retest with `make qemu EL2=1`;
- `board/applem1` reuses the same entry code (only the FDT copy address and the GIC line change).

**Lessons from building it that carry over to the M1:**
- **Never read-modify-write a write-1-to-set/clear register.** TamaGo's GIC driver did exactly that on `GICD_ICENABLER`: disabling one interrupt disabled its whole bank, and the first keystroke silently disabled the power button. AIC2's mask registers have the same semantics, so its adapter must write single bits.
- **`goos.Idle(until)` gets `until == 0` for "no timer pending",** not "due now". Treating it as due spun the CPU.
- **Device registers: single 32-bit atomic loads and stores.** That's what hypervisors (and m1n1's hypervisor mode) can decode.
- **QEMU 11.1.x HVF WFI bug.** After the first device interrupt, idle vCPUs spin (QEMU issue 959). Guest state is clean, and TCG and real hardware are unaffected. `board/virt` polls the UART under HVF to avoid it.
- **Running tests 50× under the race detector found most of the real bugs.** Keep doing it for M1 drivers.

### 3.3 After the first native boot: keyboard, idle, watchdog, storage (built 2026-11-25, keyboard and trackpad confirmed on the Mac 2026-11-26)

**Second native boot (2026-11-26, `screens/IMG_2342.JPG`): keyboard and trackpad work.** What the autorun's `lsdev idle watchdog hid` showed, about 5 s after boot:

| Row | On the Mac | Reading |
|---|---|---|
| `hid-transport@0` | `ready pin`, 10 power domains (0 switched on), 0 pins muxed, 8 MHz, 121 packets (20 keyboard, CRC ok 121), 0 empty, 9 keys (0 dropped), 0 restarts | The ready line works, so no blind-read fallback. m1n1 had already powered the domains and muxed the pins, so godeep had nothing to switch. Every packet passed CRC |
| trackpad (same row) | `98 reports`, `multitouch on: 1 sent, status ok`, `booted packets 1, 1 replies` | The multitouch write was accepted on the first try |
| `idle` | WFE + event stream every 683 µs: 53.5% idle, 796 wake-ups/s | WFE sleeps (a spinning core would show millions of wake-ups). The 53.5% is averaged since boot, over the autorun output scrolling a 3456×2234 framebuffer in software. **To check:** `lsdev idle` after a quiet minute should be well above 90% |
| `watchdog@2922b0000` | armed, 30s timeout, bites in 25.003s | Armed and petted. `ps` at 22.9 s showed every process running. **To check:** that it stays up past 30 s |

Typing worked: `Darian is Hickman` → `Darian: command not found`, `ls` → its usage line, then `ps` (7 processes, `drv/hid-transport@0` and `drv/idle` among them).

**How it was built (2026-11-25).** QEMU can't emulate Apple hardware, so each piece is host-tested against a model of the hardware (exact register sequences), wired up and exercised on QEMU sbsa-ref through `tools/fakem1n1` (RAM-backed stand-ins for the Apple registers), and reports what it did in `lsdev`, so one photo of the screen says how far it got.

| Piece | Where | What it does | How to read it on the Mac |
|---|---|---|---|
| **Idle** | `board/applem1/idle.go` | WFE woken by the generic timer's **event stream** (`CNTKCTL_EL1.EVNTEN`, ~0.7 ms): needs no interrupt controller and no Apple timer-FIQ setup. The core sleeps to the next Go timer instead of spinning | `idle` row: ~1,500 wake-ups/s means WFE sleeps; millions means it spins (QEMU TCG's WFE never sleeps) |
| **Watchdog** | `drivers/apple/wdt` (after Linux `apple_wdt.c`) | WD1 armed at 30 s, petted every 10 s: a kernel that stops scheduling reboots. `reboot` resets through it (`dev.Rebooter`). Stopped on `halt` and on fault screens, so they stay readable | `watchdog@2922b0000` row: `armed, 30s timeout, bites in …` |
| **Keyboard** | `drivers/apple/{pmgr,gpio,spi,spihid}` (after U-Boot's polled `apple_spi_kbd.c`, Linux `spi-apple.c`, `pinctrl-apple-gpio.c`, `apple-pmgr-pwrstate.c`) | from the tree: power domains (8, parents first), spi3 pin mux, SPI at 8 MHz, spien pulse 1-0-1; then polls the keyboard's ready line (nub GPIO 6) every 5 ms, or reads blind every 10 ms if it never asserts within 3 s. Decodes 256-byte packets into keys: US layout, Shift, Caps Lock, Ctrl (Ctrl-C kills the foreground program), arrows, repeat. Bytes go into the console after autorun | `hid-transport@0` row: mode (`ready pin` / `blind reads`), power domains switched on, pins muxed, packets (keyboard, CRC ok), empty reads, keys. A `first other packet` hex dump appears if packets aren't keyboard reports |
| `kernel/fdt` phandles | `Refs`, `ByPhandle`, `TranslateAddr` | resolves `power-domains`, `*-gpios`, `interrupts-extended`, `pinctrl-0`, `clocks` | — |
| `dev.Node.Config` | `kernel/dev` | board-resolved driver settings, or the error that prevented them (shown in `lsdev` instead of lost) | `failed` rows carry the reason |
| **Trackpad** | `drivers/apple/spihid` (`trackpad.go`), `kernel/input` | after the booted packet (or 1 s), one write: the multitouch set-report (device 2, report 0x02 = 01, with both CRCs; the device answers `ac 27 68 d5`). Reports: 6-byte mouse header, 38-byte touch header, 30-byte contacts, reassembled across packets. One finger → pointer motion, two → wheel, click → button 0, every contact → touch events | `hid-transport@0` row: `trackpad: N reports (multitouch), multitouch on: 1 sent, status ok`; `evtest` prints the events. `first unrecognized trackpad report` shows hex if the format differs |
| `make native` | Makefile | host tests → `bin/godeep-applem1.img` → `check-applem1` → `tools/godeep-partition.sh install` (asks to type `godeep`, and for sudo) | — |

Also from this round: on QEMU, typing now reaches the Mac image's shell (`drivers/arm/pl011` polls the serial port into the same input channel), and `check-applem1` types `help` and expects the shell's answer.

**Built on QEMU virt, for the Mac's SSD later.** `make check-virt` boots headless under HVF and reads both partitions of an NVMe disk from the shell:

| Layer | Package | Status |
|---|---|---|
| PCIe | `drivers/pci` | ECAM host bridge: scans bus 0, sizes and assigns BARs from `ranges`, enables decoding and bus mastering, adds each function as a device named by class (`nvme0`), compatible `pciclass,CCSSPP`. Apple's root complex needs its own bring-up, then this applies |
| NVMe | `drivers/nvme` | polled; admin + one I/O queue pair in the DMA region, identify, reads with PRP lists, split at MDTS; one goroutine owns the controller. This is the command layer ANS2 shares (§2) |
| Block | `kernel/block` | `block.Device`, GPT (CRC-checked) and MBR, a cached `io.ReaderAt` |
| Filesystems | `fs/fat`, `fs/ext4` (over `fs/rofs`) | read-only `io/fs.FS`: FAT12/16/32 with long names; ext2/3/4 with extents, block maps, 64-bit, htree, symlinks. Tested on images made by `mke2fs`/`mkfs.fat` and with `testing/fstest` |
| Shell | `lsblk`, `ls DISK:PATH`, `cat DISK:PATH` | no mount table: `cat nvme0p2:/hello.txt` |
| USB | `drivers/usb/xhci`, `drivers/usb` | polled xHCI: controller, rings, port reset, addressing, descriptors, HID boot keyboard (types into the console) and mouse (`evtest`); hubs and USB sticks since §3.4. For the Mac's USB-C ports once their Apple-specific setup exists |

### 3.4 The QEMU build-out, and what carries to the Mac (2026-11-26)

Built and checked on QEMU (`make check-virt`, under HVF and TCG), tested on the host 50× under the race detector. Everything is in the Mac image too (`bin/godeep-applem1.img`, about 12 MB now); the right column says what each piece needs there.

| Piece | Where | On QEMU | What it needs on the Mac |
|---|---|---|---|
| virtio transport, virtqueues (with chains) | `drivers/virtio/mmio` | modern virtio-mmio; host-tested against a device model | nothing: QEMU only |
| Entropy | `drivers/virtio/rng` → `board/virt` DRBG | seeds `crypto/rand` at start | `/chosen` seed (§2, XS) |
| Network | `drivers/virtio/net`, `net/netstack` (gVisor), `kernel/net.go` (`netd`), DHCP + DNS | IP by DHCP, `ifconfig`, `host`, `get http://` | **a NIC**: DWC3 gadget + CDC-ECM (M4) |
| SSH | `programs/sshd` | pty sessions on the ordinary shell, `exec`, SFTP/scp, kill-on-disconnect | a NIC, or the UART under m1n1's hypervisor (`SSHServer.md` §2.3) |
| Files | `kernel/vfs`, `fs/memfs` (`ram:`), `fs/ninep` + `drivers/virtio/p9` (`host:`) | `df`, `cp`, `mv`, `rm`, `mkdir`, `write`; the repo shared as `host:` | `ram:` works as is; host files over 9P need the network |
| WASM programs | `programs/wasm` (wazero interpreter) | `scp prog.wasm godeep:` or `host:/…/prog.wasm`, run with no reboot; `/ram` writable | works as is; getting programs onto the Mac needs the network (or GODEEPDATA once the SSD driver exists) |
| Kernel log, profiles | `kernel/klog` (`dmesg`), `pprof` builtin | `ssh godeep dmesg`, `ssh godeep pprof heap > heap.pb.gz` | works as is (`dmesg` on the screen) |
| USB hubs and sticks | `drivers/usb/xhci` (`hub.go`, `storage.go`) | a stick behind a hub: `lsblk`, `cat usbdisk0p2:/…` | DWC3 host setup, ATC PHY, CD321x, transaction translators (§2) |
| Console + pointer | `drivers/simplefb` (scrollback, selection, pointer), `kernel/inputd.go`, `drivers/qemu/fwcfg` (ramfb) | the USB mouse selects and pastes on QEMU's framebuffer | **wired** on the Mac: the trackpad drives m1n1's framebuffer console; untested on hardware |
| Dev loop | `make hot`, `tools/qemucheck` `local:` and `-http` steps | rebuild + reboot on save | the two-computer loop (`Using2Computers.md`) |

### 3.5 Drivers the Mac's device tree asks for (from `lsdev`, 2026-11-25/26)

Progress is logged in `TargetMyMacProgress.log` (append-only). Both native boots printed `lsdev` with every node m1n1's tree describes (`screens/IMG_2341.JPG`, `IMG_2342.JPG`). The start of the list scrolled off the screen; the next boot can show all of it (two-finger scroll goes back through the console's history), and a text copy will come over the UART or SSH. What the photos show as **"no driver"**, with what each is:

| Node(s) | Compatible | What it is | Status / plan |
|---|---|---|---|
| `serial@39b200000` | `apple,s5l-uart` | Samsung debug UART | **next** (§2): console over m1n1's hypervisor |
| `smc@290400000` | `apple,t6000-smc` | System Management Controller (RTKit) | M3: power-off, power button, lid, battery, RTC |
| `sart@393c50000` | `apple,t6000-sart` | the SSD's address filter | M5, with ANS2 |
| `usb@702280000`, `usb@b02280000`, `usb@f02280000` | `apple,t6000-dwc3` | the USB-C controllers (host and device) | M4 (device mode, network) and host mode (xHCI above it is built) |
| `usb-pd@38`, `@3a`, `@3b`, `@3f` | `apple,cd321x` | USB-C power-delivery/port controllers (on I²C) | with DWC3: role switching |
| `spmi@2920a1300` | `apple,t6000-spmi` | bus to the PMIC | S; boot flags, maybe the RTC |
| `sio@39bc00000`, `sio-firmware-data@…` | `apple,t6000-sio`, `apple,asc-mem` | DMA coprocessor for audio and DisplayPort | M6 |
| `spi@39b104000`, `spi@39b10c000` | `apple,t6000-spi` | other SPI buses (the keyboard's is driven through `hid-transport@0`) | as needed |
| `fpwm@…` | `apple,t6000-fpwm` | keyboard backlight PWM | XS |
| `wifi@0` | `pci14e4,4433` | BCM4387 Wi-Fi, behind PCIe | M6 (PCIe root complex first) |
| `power-controller@…` (dozens), `power-management@…` | `apple,t6000-pmgr-pwrstate`, `-pmgr`, `-pmgr-misc` | power domains | driven as a library (`drivers/apple/pmgr`) by the drivers that need them; no standalone driver planned |
| `region@…`, `report@…` | `apple,asc-mem`, `apple,t6000-pap-v2-report-entry` | coprocessor memory regions, PAP report entries | consumed by their coprocessor drivers; no driver of their own |

Learned on QEMU and needed for the Mac too: **transaction translators** for low/full-speed devices behind high-speed hubs (any USB-C dock), **real entropy** (the `/chosen` seed), and **DHCP lease renewal** for long sessions over the cable.

---

## 4. TamaGo blockers specific to this Mac

These must be fixed before godeep can print a single character on the real hardware:

1. ✅ **Entry at EL2.** *Decided (§3.2: Option A, drop to EL1) and done (§3.1): `board/virt/entry_arm64.s` drops EL2 → EL1 and is tested at EL2 in QEMU. Small follow-up: write `VTTBR_EL2 = 0` explicitly. `board/applem1` reuses it.* Background: m1n1 starts payloads at **EL2** (Asahi kernels run in EL2/VHE). `tamago/arm64` `cpuinit` handles EL1 or EL3 only: from EL2 it would execute EL3 register writes, which is an undefined-instruction fault. Add an EL2→EL1 drop (set `HCR_EL2.RW`, then `ERET` to EL1). That's about 15 lines of assembly in a board-specific `linkcpuinit` override.
2. ✅ **RAM is above 1 TiB.** *Done: `kernel/pgtable` (4-level, 48-bit) plus an early identity map in `cpuinit`; the 4-level path runs daily on `board/virt`, RAM at 1 TB is first exercised on the Mac.* Background: DRAM starts at `0x100_0000_0000`. TamaGo's arm64 MMU sets up a 39-bit VA with a 40-bit physical range and flat mapping, which can't reach it. Either patch the MMU (48-bit VA, 4-level tables) or map RAM at a low virtual address. Flat mapping plus a 48-bit VA is the simpler patch. The DMA region (§3) must sit in RAM the Go heap doesn't own, exactly as `board/virt` does it.
3. **AIC, not GIC.** *Not needed yet:* `board/applem1` idles in WFE (timer event stream), and the keyboard is polled (§3.3). AIC2 (an `irq.Controller` adapter, plus checking how the timer FIQ reaches EL1 under m1n1) replaces the polling and allows WFI on a timer deadline: fewer wake-ups, deeper sleep.
4. **No PSCI.** ◐ *Reboot done:* `reboot` resets through the watchdog (§3.3). Power-off needs SMC (M3); until then `halt` stops godeep with the screen kept, and you hold the power button.
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

**Easy way:** `tools/godeep-partition.sh` does all of this, much like the Omarchy installer. It checks the machine and shrinks Macintosh HD if there isn't already enough free space. Then it runs the Asahi installer with its menus answered for you: the OS list is cut down to a single godeep entry (the "UEFI environment only" boot chain, named `godeep`), so all you do is press Enter and type your passwords. After the reboot, `tools/godeep-partition.sh finish` adds GODEEPDATA and copies `bin/godeep-arm64.img` onto godeep's boot partition as `godeep.img`; rerun it whenever you rebuild. It also has `--dry-run`, `status` and `undo`, and every disk change asks you to type `godeep` first. The steps below are what it does, for reference or for doing it by hand.

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

**Choosing the size.** The godeep stub (2.5 GB) and its ESP (0.5 GB) need about 3 GB. The rest becomes GODEEPDATA, for godeep's future disk driver and filesystem work (`ReUsingGVisor.md` §5) and as a drop box shared with macOS and Omarchy. 12 GB gives about 9 GB of data space. Use 4 GB if you only want the boot entry.

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

### 6.8 Step 5: put godeep on its ESP and boot it

The Mac image now exists (§3.1). It boots to the shell under QEMU with an m1n1-style hand-off (`make check-applem1`, below), and booted on this Mac on 2026-11-25: m1n1 → godeep at EL2, autorun `lsdev`/`ps`/`mem`/`uptime` on the built-in display (`screens/IMG_2341.JPG`). The second boot on 2026-11-26 had a working keyboard and trackpad (`screens/IMG_2342.JPG`, §3.3).

**Easy way:** `tools/godeep-partition.sh finish`, or `tools/godeep-partition.sh install` to update godeep later. It builds `bin/godeep-applem1.img` if it's missing or stale, copies it to the godeep boot partition as `godeep.img` (keeping the previous one as `godeep.img.prev`), and **makes m1n1 boot godeep directly, without U-Boot**.

**Why not U-Boot.** The first native boot (2026-11-25) showed U-Boot autobooting **Omarchy** from the godeep entry: U-Boot's standard boot scans *every* NVMe partition and boots the first boot method that works anywhere on the disk. Omarchy's ESP (with its EFI loader) won over godeep's `extlinux.conf`. Whether that was because of scan order, Omarchy's EFI boot variables, or `booti` failing on godeep and the scan moving on wasn't visible on screen. So the godeep ESP's `m1n1/boot.bin` (m1n1 stage 2 followed by its payloads: `m1n1 | dtbs | gzip(U-Boot)`) is rebuilt by `tools/m1n1boot` with `gzip(godeep Image)` in U-Boot's place. m1n1 then picks this Mac's device tree and jumps straight to godeep at EL2, using the same Image boot protocol U-Boot uses (self-relocation, §3.1, covers m1n1's choice of load address). U-Boot's `boot.bin` is kept as `m1n1/boot.bin.uboot`. `--uboot` puts it back and uses `extlinux.conf` autoboot. `--no-autoboot` puts it back and stops at U-Boot's prompt for the manual commands below, which are also saved as `GODEEP-BOOT.txt`.

Check a `boot.bin` by hand: `go run ./tools/m1n1boot -info boot.bin` reports whether its kernel payload is U-Boot or godeep.

**The ESP needs root to mount** (`sudo diskutil mount`). Its `diskN sN` number can **change after a reboot**: partitions added in a session get temporary identifiers, so on this Mac the godeep ESP showed as `disk0s10` right after partitioning and `disk0s4` after a reboot. Run `status` to get the current one.

**Rehearse in QEMU first: `make check-applem1`.** QEMU's `sbsa-ref` machine has RAM at 1 TB, the same base as this Mac's DRAM, so the *same* `bin/godeep-applem1.img` runs there unmodified. `tools/fakem1n1` hands it over the way m1n1 does: entry at EL2 with E2H=1, loaded away from its link address (so it self-relocates), and an m1n1-style device tree with the panel's 3456×2234 `x2r10g10b10` framebuffer. The check boots to the shell and saves the screen as a PNG. It also forces a fault at stages 1 and 3 and checks that the red fault screen appears. `make qemu-applem1` runs the same thing interactively. Its first run found a bug that would have stopped every native boot: TamaGo's heap address limit ended exactly at 1 TB (third_party/tamago/GODEEP_PATCHES.md, patch 3).

What to expect on the Mac: the framebuffer clears to dark blue-grey, the boot banner and the `board:` line appear (it says `entered at EL2 (VHE, E2H=1)`), then the autorun output (`lsdev` with this Mac's devices from m1n1's tree, then `ps`, `mem`, `uptime`). About 5 s later, `lsdev idle watchdog hid` prints three rows that say whether idle sleeps, the watchdog is armed, and how far the keyboard got (§3.3); then type. If the Mac reboots by itself about 30 s in, the watchdog fired: godeep stopped scheduling. If U-Boot's text or another OS appears, you didn't boot the godeep entry.

**One command does it all:** `make native` runs the host tests, builds the image, rehearses it on QEMU, then installs it (asks you to type `godeep`, and for sudo).

**If it stops before the console**, read the top-left corner of the screen. godeep draws a numbered square for each boot stage it reaches, on m1n1's framebuffer:

| Last square | Stage reached | So it stopped in |
|---|---|---|
| none (m1n1's output or the Apple logo stays) | godeep never ran Go code | `entry_arm64.s`: relocation, EL2→EL1, the early identity map; or no framebuffer in the tree |
| 1 blue | Hwinit0: Go code runs, device tree read, framebuffer mapped | building or switching to the final page tables |
| 2 green | final page tables live | Go runtime setup (`schedinit`: the heap), or TamaGo's `CPU.Init` |
| 3 yellow | Hwinit1: CPU, caches, timers | runtime start: goroutines, `main`, the kernel's init |
| 4 purple | Go runtime up, board setup started | console creation (`drivers/simplefb`) |

A **red screen** titled `GODEEP EARLY FAULT` means the CPU took an exception. Take a photo: it shows the stage plus **ESR** (why: e.g. `96000004` is a data abort, translation fault), **ELR** (the faulting instruction: look it up with `go tool objdump` on an unstripped build) and **FAR** (the address it touched). Then a second computer with m1n1's hypervisor (§5 option A) can show the rest. Hold the power button to get back.

With U-Boot restored (`--no-autoboot`), boot the godeep entry and, at the U-Boot prompt:

```
=> load nvme 0:<godeep-ESP-partition> ${kernel_addr_r} godeep.img
=> booti ${kernel_addr_r} - ${fdtcontroladdr}
```

U-Boot numbers partitions by GPT entry; `part list nvme 0` shows them. The godeep ESP is the 500 MB EFI partition right before GODEEPDATA.

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
| Booting godeep shows U-Boot, then Omarchy | U-Boot is still on the godeep ESP, and its disk-wide scan found Omarchy first. Run `tools/godeep-partition.sh install` (m1n1 boots godeep directly) |
| Booting godeep stays on the Apple logo or m1n1's output | godeep stopped before its console came up (§6.8) |
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
| Boot framebuffer | `ramfb` / virtio-gpu (`GoGpuSupport.md`) |

### 7.3 Borrow what isn't native yet

While a block has no native driver, something else covers it. Each row says what stands in, and what retires it.

| Need | Stand-in until the native driver lands | Retired by |
|---|---|---|
| Everything (daily development) | **QEMU `board/virt`**: virtio net/blk/9p/rng/gpu, PL011, RTC | each native driver, one at a time; QEMU stays for tests and CI |
| CPU boot, MMU, timer, caches | **TamaGo `arm64`** (with the §4 patches) | stays: this *is* the native driver |
| TCP/IP, SSH | **built**: `net/netstack` (gVisor), `programs/sshd` (`x/crypto/ssh`, SFTP) | stays: only the NIC underneath changes. TLS needs root certificates (not yet) |
| Screen | **firmware framebuffer** that iBoot/m1n1 already set up | DCP driver (maybe never: the framebuffer is enough) |
| Console | framebuffer text out; autorun as input; **Samsung debug UART** in/out via a second Mac or the m1n1 proxy | SPI-HID keyboard and trackpad (M2, polled: works on the Mac since 2026-11-26) |
| PCIe links, some clocks and power domains | **m1n1** brings them up before jumping to godeep | native PMGR/PCIe drivers, when needed |
| Networking on the Mac | **USB-C gadget Ethernet**: a DWC3 device-mode driver with CDC-ECM framing, publishing a `netdev.NIC` (everything above it is built); meanwhile SSH over the UART under m1n1's hypervisor | Wi-Fi (maybe never) |
| Files and storage | **GODEEPDATA** (§6.6) read by Omarchy/macOS; `ram:` (built); **9P** from the other computer (client built, `fs/ninep`; over the network it needs a 9P-over-TCP transport); USB sticks once DWC3 host mode works | native NVMe (M5): block layer, FAT/ext4 and the NVMe command layer are built on QEMU |
| Entropy | **the `/chosen` seed** from iBoot/m1n1 seeding the DRBG (**not done yet**: the Mac board still seeds from the boot counter) | stays (no hardware RNG reachable on M1 Pro) |
| Power off / reboot | **watchdog** reboot (built) | SMC power-off (M3) |

Two strategies from this table matter most:
- **Lean on the firmware.** iBoot and m1n1 already initialized the display, memory, PCIe and more. Using that state instead of re-initializing it removes whole drivers from the critical path. Linux on Apple Silicon started the same way.
- **Get a network early (M4), then use the Mac as a peripheral.** Once godeep has an IP over a USB-C cable, files (9P), logs, `pprof` and even storage can come from the Mac until native drivers exist. That turns one hard driver (DWC3) into access to everything.

### 7.4 Proposed structure (small additions, mostly built)

These keep native drivers testable on the host and shareable between boards:

| Addition | Purpose |
|---|---|
| ✅ `kernel/mmio`: a `Regs` interface (`Read32`/`Write32` at an offset) with a real implementation (single atomic 32-bit accesses) and a **recording fake** that can assert or replay traces | rung 2; enforces the "no read-modify-write of W1S/W1C registers" lesson in one place. *Built.* |
| ◐ `drivers/<vendor>/<device>`: driver code as ordinary Go packages, no build tags | host tests run on every driver; boards only wire them up. *Built: `drivers/arm/{pl011,pl031,pl061}`, `drivers/virtio/{mmio,net,rng,p9}`, `drivers/qemu/fwcfg`, `drivers/pci`, `drivers/nvme`, `drivers/usb/xhci` (with hubs and mass storage), `drivers/simplefb`, `drivers/apple/{wdt,pmgr,gpio,spi,spihid}`. Next: `drivers/apple/{uart,asc,smc,dart,aic,ans,sart,dwc3}` (§3.5)* |
| ✅ `board/applem1`: build-tagged `tamago && arm64 && applem1`; device list from the **FDT** m1n1 builds from the ADT (via `kernel/fdt`); registers the Apple drivers | the third board, next to `board/virt` |
| ◐ Service interfaces as needed: ✅ `block.Device` (`kernel/block`), ✅ `dev.Rebooter`, ✅ `netdev.NIC` (`kernel/netdev`), ✅ `input.Source`, ✅ `vfs.Named` (volumes); later `dev.Framebuffer`. (The keyboard feeds the console's byte channel too.) | consumers (filesystems, netstack, shell) never depend on a specific driver |
| `make test` stays host-only; `make qemu` (✅ Image + device tree) and `make qemu EL2=1` (✅ m1n1-style entry) run the rung-3 checks; a `make m1-guest` target wraps m1n1's `run_guest.py` | one command per rung |

### 7.5 Definition of done for a native driver

A row in §2 moves from ✗/◐ to ✅ when the driver has:
1. host tests (rungs 1–2) passing 50× under the race detector;
2. a QEMU-analog run (rung 3) with `lsdev` showing it `ready`, `kill <drv pid>` followed by a clean restart, and idle CPU unchanged;
3. an m1n1-hypervisor run (rung 4) whose MMIO trace was reviewed against the Asahi/Linux driver's behavior, with the trace saved as a rung-2 test fixture;
4. a native-boot run (rung 5);
5. its §2 row, `DriverModel.md` and any new service interface updated.

### 7.6 What's still worth building on QEMU, and what needs two machines

**The rule of thumb:** if QEMU emulates the device or protocol (virtio, xHCI, NVMe, USB devices, PL011, a network), build it on QEMU, where `make hot` and `check-virt` make each try take seconds. If it's Apple silicon IP (anything with an `apple,` compatible), QEMU can't help beyond host tests against `mmio.Fake`: it needs the Mac as the target and a second computer driving it (`Using2Computers.md`), because the only way to see what the hardware does is m1n1's hypervisor (MMIO traces, a serial console, a shell at a fault).

`board/applem1` on QEMU sbsa-ref (`tools/fakem1n1`, `make check-applem1`) sits in between: it keeps the Mac image's boot path, framebuffer, fault screen and autorun from regressing, but its Apple nodes are RAM pages that don't behave like hardware.

#### Still worth building on QEMU (portable; carries to the Mac)

| What | Why it pays off on the Mac | QEMU test bed | Size |
|---|---|---|---|
| **FAT write support**, then NVMe writes | GODEEPDATA (§6.6) becomes a place godeep can save files that macOS and Omarchy read; ANS2 shares the NVMe command layer | writable NVMe/USB disk images in `check-virt`, `fsck.fat` on the host afterwards | M |
| **USB CDC-ECM, host side** (`usb-net`) | the same Ethernet-in-bulk-transfers framing the DWC3 gadget will use, through the same `netdev.NIC` → `netd` path; and USB Ethernet dongles work on the Mac once DWC3 host mode does | QEMU `-device usb-net` on the xHCI, SSH over it | S–M |
| **9P over TCP** (and 9P writes) | the Mac reads and writes the dev machine's tree over the USB-C network: the way programs and files get onto the Mac before the SSD driver exists | `fs/ninep` over a TCP transport against a host 9P server (`u9fs`/`diod`) through QEMU's NAT | S |
| **TLS roots** (an embedded CA bundle) | `get https://`, Go module/proxy fetches, any HTTPS service | QEMU NAT to the internet | XS–S |
| **DHCP lease renewal** | sessions over the cable that outlive a lease | short leases from a test DHCP server (host test exists) | XS |
| **USB storage robustness**: REQUEST SENSE, reset recovery, writes | real USB sticks stall and need recovery more than QEMU's | `usb-storage` with error injection in host tests; writable images | S |
| **Keyboard layouts**, and the shell's line editing (history, cursor keys) | used every day on the Mac's keyboard | serial and USB keyboards on QEMU, host tests | S |
| **WASM**: writable host share, faster startup (cached compiled modules), more WASI | the dev loop on the Mac is "push a program, run it" | `host:` and `ram:` on QEMU | S |
| **Kernel services**: `ipc` (named ports), tightening capabilities (sessions get narrower `cap`s), `top`/accounting, klog persistence to GODEEPDATA | all board-independent | QEMU and the hosted build | M |
| **SMP scheduling groundwork** | the Mac has 10 cores; TamaGo uses one. The Go side (per-CPU idle, timers, starting secondary cores into the runtime) can be done on QEMU via PSCI `CPU_ON`; only the Mac's core start (PMGR `cpu-start`) is Apple-specific | QEMU `-smp 4` | L |

#### Only makes sense with two machines (Mac as target, the other as developer)

| What | Why QEMU can't do it | What the second machine gives |
|---|---|---|
| **Samsung UART** (`serial@39b200000`) | the logic is host-testable, but its output is only visible on the Mac | the guest UART forwarded as `/dev/ttyACM1`: the console as text, then SSH over it |
| **AIC2** interrupt controller; keyboard and timer on interrupts | Apple IP; how the timer FIQ reaches EL1 under m1n1 has to be observed | MMIO traces of the ack loop, a shell when an interrupt storms |
| **DART** IOMMU, **ASC mailbox**, **RTKit** encodings (verified, not just tested against the fake) | Apple IP; RTKit firmware only exists on the Mac | traces of the mailbox, the coprocessor's crash logs |
| **SMC** (power-off, power button, lid, battery, maybe RTC) | needs RTKit on real firmware | see above; power-off is otherwise untestable |
| **ANS2 + SART** (the SSD) | Apple's NVMe variant and its address filter | traces before any write; **read-only until verified** |
| **DWC3** host and device mode, **ATC PHY**, **CD321x** (on I²C) | Apple's USB-C glue around a standard core; QEMU's xHCI already covers the standard part | the gadget network link itself, then everything in §3.4 over it |
| **Transaction translators** behind high-speed hubs (USB-C docks) | QEMU's `usb-hub` is full speed: there's nothing to translate | a real dock with a keyboard behind it |
| **SPMI**, keyboard-backlight **PWM**, **SIO**, Wi-Fi | Apple IP | traces, and eyes on the hardware |
| **Native measurements**: idle cost, interrupt latency, watchdog over long uptimes, the entropy seed's presence in `/chosen` | only real silicon answers | text output to compare runs, instead of photos |
| **MMIO trace fixtures** for every Apple driver (§7.5, rung 4 → rung 2) | traces come from the hardware | `run_guest.py` logs, cut into `mmio.Fake` tests that then run on every `make test` |

**So the split from here:** keep QEMU for whatever has an emulated device or is pure kernel code, and keep the Mac sessions for Apple IP. The first two-machine session should be the UART, because every later Apple driver is easier to debug with text output instead of a photo.

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
| **M1: hello on the laptop screen** ✅ | `board/applem1`: self-relocation, 4-level MMU, RAM + DMA layout, framebuffer console, autorun of `lsdev`/`ps`/`mem`/`uptime` (§3.1). **Booted on the Mac 2026-11-25** (§6.8, `screens/IMG_2341.JPG`). Then WFE idle and watchdog/`reboot`, confirmed on the Mac 2026-11-26 | TamaGo arm64, generic timer, godeep's EL2 entry, Image format and FDT device list (M0.7), `x/image/font` | done: relocation, `kernel/pgtable`, `drivers/simplefb`, `board/applem1` |
| **M2: interactive** ◐ | Keyboard via SPI-HID as a managed driver, polled (no AIC): the godeep shell on the built-in screen and keyboard. **Works on the Mac (2026-11-26, `screens/IMG_2342.JPG`)**: typing, and the trackpad in multitouch mode. Then AIC2 (`irq.Controller`) to replace polling and spinning-free WFI | `kernel/irq`, `kernel/dev`, `kernel/fdt` phandles, godeep `tty`, `sh` | done: `drivers/apple/{pmgr,gpio,spi,spihid}`; next: AIC2 |
| **M3: coprocessors** (next) | ASC mailbox (`rtkit.Mailbox`) + DART (`dma.IOMMU`); verify the RTKit encodings; SMC (battery, lid, power button, power-off) as the first client | `kernel/rtkit`, `kernel/dma` | ASC mailbox, DART, SMC protocol |
| **M4: networking** ◐ | DWC3 device mode + CDC-ECM, so godeep gets an IP over a USB-C cable; SSH into it | gVisor, `x/crypto/ssh` | done on QEMU (§3.4): `net/netstack`, DHCP/DNS, `netd`, `programs/sshd` with SFTP, WASM loading; next: the DWC3 gadget driver (a `netdev.NIC`) |
| **M5: storage** ◐ | ANS2 NVMe on RTKit + SART, read-only; read the Omarchy ext4 partition; use GODEEPDATA | `kernel/rtkit`, `kernel/dma` | done on QEMU (§3.3): `drivers/nvme` command layer, `kernel/block` (GPT), `fs/fat`, `fs/ext4`, `lsblk`/`ls`/`cat`; next: ANS2's differences, SART, RTKit boot (after M3) |
| M6: everything else | SMP, Wi-Fi, audio | — | large |

- **Order from here (2026-11-26, after the QEMU build-out):** the keyboard, trackpad, idle and watchdog work on the Mac. Two checks are left from that boot: steady-state idle (`lsdev idle` after a quiet minute) and the watchdog past 30 s; the next boot also tries the trackpad pointer and scrollback on the console, and the `/chosen` entropy seed (XS). Then the second-computer loop (`docs/Using2Computers.md`, starting with `drivers/apple/uart`), M3 (mailbox, RTKit check, SMC), then M5's Apple layer, which connects the storage stack already running on QEMU to the real SSD. AIC2 when polling's cost matters. Without a second computer for m1n1's hypervisor, each native test is a reboot and a photo, so every driver reports its progress in `lsdev`.
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
- godeep: [`DriverModel.md`](DriverModel.md) (the §3 foundation), `board/virt` (the QEMU arm64 board an M1 board would sit next to), `GoGpuSupport.md`, `ReUsingGVisor.md`
