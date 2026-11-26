# Porting XNU to Go: What Would Change in godeep?

This document assesses what would change in `godeep` if it had been started by porting Apple's open-source **XNU** kernel (`apple-oss-distributions/xnu`) to Go instead of building a Go-native, single-address-space kernel on TamaGo. It follows the same lens as [`ReUsingGVisor.md`](ReUsingGVisor.md): what XNU actually contains, what it *doesn't* contain for an Apple M1 Pro, how it clashes or aligns with godeep's architecture, and which parts are still worth borrowing as design or hardware references.

---

## TL;DR

- **The biggest surprise: open-source XNU does *not* include Apple Silicon peripheral drivers.** AIC2, DART, SART, ASC/RTKit, SMC, ANS2 NVMe, DWC3/ATC/CD321x USB-C, PMGR, GPIO, SPI-HID keyboard/trackpad, DCP display, AGX GPU, BCM4387 Wi-Fi, and APFS are all **closed-source kexts** outside XNU. Starting from XNU would **not** have saved any of the M1 driver work in [`TargetMyMac.md`](TargetMyMac.md) — Asahi Linux and m1n1 remain the only open-source reference for those peripherals.
- **Porting XNU would invert godeep's core premise.** XNU is a ~1.5–2M LOC dual-mode (EL0/EL1) Mach + BSD + C++ IOKit kernel built around hardware address spaces, preemptive Mach threads, `copyin`/`copyout`, Mach-O binaries, and POSIX vnodes. Porting it to Go would fight TamaGo's single-address-space runtime and Go's garbage collector at every layer, throwing away the zero-context-switch Go-concurrency process model.
- **Two areas of XNU *are* genuinely valuable to godeep:**
  1. **Apple Silicon CPU & early-boot reference (`osfmk/arm64/`, `pexpert/`):** real Apple CPU errata, `HID*` registers, 16 KB page table geometry, timer FIQ setup, `pe_serial.c` (Samsung UART, directly relevant to milestone **N3**), SMP bring-up, and the iBoot `boot_args`/ADT layout.
  2. **Capability & accounting designs (`osfmk/ipc/`, `osfmk/kern/ledger.c`, `iokit/`):** Mach ports (unforgeable capability rights + dead-name notifications) are philosophical cousins to godeep's planned `kernel/ipc` (**Q6**), Mach ledgers are a clean blueprint for per-`Proc` resource quotas, and IOKit's `IOWorkLoop`/`IOCommandGate` validates godeep's single-owner-goroutine driver rule.
- **Verdict:** **Do not restart godeep as an XNU-to-Go port.** Keep godeep's Go-native core and Asahi/m1n1 driver references, and mine XNU selectively for CPU-level Apple Silicon details and Mach IPC/ledger design patterns (clean-room, avoiding APSL 2.0 derivative entanglement).

---

## 1. What open-source XNU actually contains (and what is missing)

A common assumption is that because macOS runs natively on Apple M1 Pro and XNU is open source, porting XNU gives you working M1 Pro drivers. In practice, macOS splits the kernel core (open source in `xnu`) from almost all hardware drivers and filesystems (closed-source binary kexts in the Kernel Collection):

```
┌─────────────────────────── macOS Kernel (Apple M1 Pro) ───────────────────────────┐
│ Open source in apple-oss-distributions/xnu (APSL 2.0, ~1.5–2M LOC)                │
│   osfmk/     Mach: tasks, threads, Mach IPC ports, vm_map/pmap (ARM64 16 KB),     │
│              Clutch scheduler, ledgers, locks, zalloc/kalloc, ARM64 CPU/MSRs      │
│   pexpert/   Platform Expert: ADT parser, boot_args, Samsung UART, early FB       │
│   bsd/       POSIX: proc, VFS/vnode/UBC, devfs/tmpfs/HFS+, netinet/mbuf/pf,       │
│              kqueue, Mach-O loader, ~500 syscalls, MAC sandbox hooks              │
│   iokit/     C++ driver framework: OSObject, IOService, IORegistry, IOWorkLoop,   │
│              IOCommandGate, IOBufferMemoryDescriptor, IOMapper (IOMMU base class) │
│   libkern/   Embedded C++ runtime, OSCollections, crypto/zlib glue                │
├───────────────────────────────────────────────────────────────────────────────────┤
│ CLOSED SOURCE (not in XNU — binary kexts only)                                    │
│   Interrupts & IOMMU:  AIC2, DART (t6000/t8110), SART                             │
│   Coprocessors:        ASC mailbox, RTBuddy/RTKit, SMC, SIO, AOP                  │
│   Storage & FS:        ANS2/ANS3 NVMe controller, APFS filesystem                 │
│   USB / PCIe / Bus:    DWC3 USB-C, ATC PHY, CD3217 PD, Apple PCIe, PMGR, GPIO,    │
│                        I²C (PASemi), SPMI, SPI, PWM                               │
│   Input / Display:     SPI-HID keyboard & multitouch trackpad, DCP, AGX GPU, ANE  │
│   Wireless / Audio:    BCM4387 Wi-Fi & Bluetooth, MCA/ADMAC/audio codecs, ISP     │
└───────────────────────────────────────────────────────────────────────────────────┘
```

**Consequence for `board/applem1`:** Had godeep started by porting XNU, you would have spent months porting Mach, BSD, and IOKit infrastructure — and at the end of it, you would *still* have to write every single M1 peripheral driver (`spihid`, `wdt`, `pmgr`, `gpio`, `spi`, `aic`, `dart`, `asc`, `smc`, `ans`, `sart`, `dwc3`) from Asahi Linux and m1n1, exactly as godeep does today.

---

## 2. Architectural clash: godeep today vs. an XNU-to-Go port

| Area | Current `godeep` | Porting XNU to Go | Impact on godeep |
|---|---|---|---|
| **Core thesis** | Go's concurrency model *is* the process model; single address space; Go memory safety replaces the MMU | Dual-mode EL0/EL1 kernel with hardware virtual memory isolation per `task` | Replaces godeep's defining simplification with a conventional Unix/Mach kernel written in Go |
| **Processes & scheduling** | `proc.Proc` = PID + `context.Context` + supervised goroutine group on Go's GMP scheduler; cooperative kill | Mach `task` + `thread` + Clutch scheduler + BSD `proc` wrapper; preemptive timer switches | You would either throw away Go's scheduler or double-schedule (XNU threads inside Go goroutines). Preemption only works for EL0 tasks, not Go kernel goroutines |
| **Memory & GC** | Single 3.5–4 GB Go heap + 512 MB explicit `kernel/dma` region; GC manages all kernel & program memory | `vm_map`, `pmap` (16 KB pages, APRR/SPRR), `zalloc`/`kalloc`, manual `copyin`/`copyout`, unified buffer cache (UBC) | Go's runtime/GC cannot manage per-process EL0 virtual address spaces; you'd end up writing non-idiomatic `unsafe` allocators everywhere and bypassing Go's safety guarantees |
| **Syscalls & ABI** | Ordinary Go method calls on capability interfaces (`cap.Net`, `*vfs.NS`); WASI via `wazero` | ~500 BSD syscalls + ~100 Mach traps + MIG RPCs + Mach-O `dyld` loader | Massive ABI surface; only justified if the goal shifts to running unmodified macOS binaries |
| **IPC & capabilities** | Go channels + unforgeable Go interface values (`kernel/ipc` planned in Q6) | **Mach Ports** (send/receive/send-once rights, port sets, `ipc_space`, notifications) | **Strong conceptual overlap.** Mach's capability model is worth borrowing directly for `kernel/ipc` without Mach's C refcounting or kernel-boundary copying |
| **Driver framework** | `kernel/dev` (`devmgr`, `drv/<name>` Procs, `mmio.Regs`, FDT `compatible` binding, channels) | **IOKit** (embedded C++ `OSObject`/`OSMetaClass` hierarchy, `IOService`, `IORegistry`, `IOWorkLoop`, `IOCommandGate`) | C++ inheritance and manual `retain`/`release` translate poorly to Go; however `IOWorkLoop`+`IOCommandGate` is already the same pattern as godeep's single-owner driver goroutine |
| **Networking** | Pure-Go gVisor `pkg/tcpip` (`net/netstack`) + `sshd`/SFTP + DHCP/DNS/HTTPS — already working | BSD `netinet`/`netinet6`, `mbuf` chains, `dlil`, `pf` (~200k LOC of C) | Porting BSD's C `mbuf` stack to Go would be months of work to replicate what gVisor already gives cleanly |
| **Storage & VFS** | `kernel/vfs` volume namespace (`VOLUME:PATH`), `io/fs.FS` + `vfs.Writable`, `memfs`, `fat` rw, `ext4` ro, `ninep` rw | BSD `vnode` VFS, namei cache, UBC page-cache integration, HFS+, devfs (no APFS) | ~100k+ LOC for POSIX semantics (`mmap`, hard links, UID/GID ACLs, UBC) unnecessary in a single-address-space Go OS |
| **Boot & QEMU loop** | Linux arm64 Image + FDT in `x0`; same kernel core runs on `board/hosted`, QEMU `virt` (HVF, ~1 s boot), QEMU `sbsa-ref` (`fakem1n1`), and M1 via m1n1 | iBoot Mach-O + `boot_args` + Apple Device Tree (ADT); 16 KB pages | Breaks direct QEMU `virt` compatibility unless you emulate iBoot/`boot_args`/ADT; slows down the sub-second host & QEMU iteration loop |
| **Code size & velocity** | ~20k LOC of idiomatic Go; interactive native M1 boot + networking/SSH/WASM/NVMe/USB/9P working today | ~1.5–2M LOC of C, C++, and assembly | Multi-year porting effort before reaching an interactive shell |
| **Licensing** | Clean Go codebase + Apache-2.0 (`gvisor`, `wazero`) + BSD (Go, TamaGo) | **APSL 2.0** (file-level copyleft, GPL-incompatible, mandatory source-modification disclosure & Apple notices) | Direct porting creates an APSL-2.0 derivative; using XNU only as a clean-room reference avoids license friction |

---

## 3. Component-by-component: what would have changed in godeep's tree

### 3.1 `kernel/proc` & `kernel/ipc` — Mach tasks, threads, and ports
- **If ported from XNU:** `proc.Table` and `proc.Proc` would split into three layers: Mach `task` (address space + `ipc_space` port table + resource ledgers), Mach `thread` (register context + kernel stack + scheduler state), and BSD `proc` (PID, parent/child, signals, file descriptor table). `ps` wouldn't parse Go's pprof goroutine labels; it would walk the Mach task/thread queues.
- **What godeep loses:** `p.Go(fn)`, cheap 2 KB goroutines as the sole execution primitive, cooperative context cancellation, and ~300 lines of crystal-clear process management in `kernel/proc`.
- **What godeep *should* borrow from XNU:**
  - **Mach Port semantics for `kernel/ipc` (Q6):** explicit capability rights (send, send-once, receive), **dead-name notifications** (when a server `Proc` or driver restarts, clients holding a port get an immediate notification channel close/event), and port transfer over messages.
  - **Mach Ledgers (`osfmk/kern/ledger.c`):** clean per-`Proc` credit/debit accounting for memory, goroutine counts, and I/O limits when implementing Phase 6 resource quotas.

### 3.2 `kernel/dev` & `drivers/*` — IOKit vs. `devmgr`
- **If ported from XNU:** `drivers/*` would sit on an `OSObject`/`IOService` inheritance tree with `OSDictionary` matching plists, `IORegistryEntry` planes, `IOWorkLoop` event loops, and `IOBufferMemoryDescriptor` scatter-gather lists.
- **Why a literal port hurts in Go:** IOKit exists largely to give C++ memory management (`retain`/`release`), RTTI (`OSMetaClass`), and serialized event loops (`IOWorkLoop` + `IOCommandGate`) in a kernel without GC or channels. Go already gives you GC, interfaces, and channels.
- **What godeep *should* borrow from XNU:**
  - **Device tree & power plane (`IORegistry` / `IOService` power states):** godeep's `dev.Node.Needs` is currently a flat string list. As M1 adds PMGR power domains, I²C (`cd321x`), and USB-C port controllers, modeling parent-child power/reset dependencies (like IOKit's power plane) will keep bring-up and clean shutdown ordered.
  - **Scatter-gather DMA descriptors (`IOBufferMemoryDescriptor` + `IOMapper`):** `kernel/dma` currently hands out contiguous buffers; once Apple DART (**N5**) maps 16 KB non-contiguous pages, an `IOMapper`-like scatter-gather mapping API will matter.

### 3.3 `board/applem1`, `kernel/pgtable`, & `kernel/fdt` — `osfmk/arm64/` and `pexpert/`
- **If ported from XNU:** `board/applem1` would speak Apple's `boot_args` struct and parse Apple's **ADT** directly (`pexpert/device_tree.c`) instead of m1n1's converted FDT (`kernel/fdt`), and `kernel/pgtable` would target 16 KB translation granules (`TG0 = 16 KB`) matching macOS's `pmap`.
- **What godeep loses:** Sharing `kernel/fdt`, `tools/mkimage`, and the Linux arm64 Image protocol across `board/virt`, `tools/fakem1n1`, and `board/applem1`. Under m1n1, Linux Image + FDT already boots natively on this MacBook Pro.
- **What godeep *should* borrow from XNU (high value!):**
  - `pexpert/arm/pe_serial.c`: Samsung UART (`apple,s5l-uart`) register definitions and FIFO handling — useful cross-check for **N3 (`drivers/apple/uart`)**.
  - `osfmk/arm64/`: Apple Silicon CPU errata, `HID*` tuning bits, generic timer FIQ behavior (relevant to **N4 / AIC2**), cache/TLB barriers, and secondary-core bring-up expectations (relevant to **N8 / SMP**).
  - `pexpert/arm64/boot.h`: `boot_args` and ADT conventions if godeep ever checks `/chosen` entropy seeds (**N2**) or boots directly from iBoot/m1n1-Darwin mode.

### 3.4 `net/`, `fs/`, & `programs/` — BSD vs. Go ecosystem
- **If ported from XNU:** You would replace `net/netstack` (gVisor), `kernel/vfs`, `fs/{memfs,fat,ext4,ninep}`, and `programs/wasm` with Go ports of `bsd/netinet` (`mbuf`s), `bsd/vfs` (`vnode` + UBC), and a Mach-O loader.
- **Assessment:** Strict downgrade for godeep. Go's standard library + gVisor `pkg/tcpip` + `x/crypto/ssh` + `wazero` already deliver working TCP/IP, DHCP/DNS/HTTPS, SSH/SFTP, writable FAT/9P/memfs, and dynamic WASI programs in pure Go, tested in seconds on the host. Even XNU's own `bsd/vfs` lacks APFS.

### 3.5 Testing & dev loop (`make test`, `make hot`, `make check-virt`)
- **Today:** Every core package and every driver in `drivers/*` builds on macOS/Linux with `make test` and runs 50× under `go test -race` against `mmio.Fake`, followed by a ~20-second headless `make check-virt` on QEMU `virt` (HVF).
- **If ported from XNU:** XNU's deep coupling between `osfmk`, `bsd`, and `iokit` (global locks, per-CPU structs, hardware `pmap` assumptions) makes host-side unit testing with `-race` dramatically harder. Preserving godeep's strict `kernel/` vs. `board/` separation is a major advantage over XNU's layout.

---

## 4. Reuse tiers: how to use XNU alongside godeep

Mirroring [`ReUsingGVisor.md`](ReUsingGVisor.md), here is the practical split for open-source XNU:

| Tier | XNU Subsystem | Verdict | How to use it in godeep |
|---|---|---|---|
| **Tier A: Hardware & CPU reference** | `pexpert/arm/pe_serial.c`, `osfmk/arm64/` (errata, timer FIQ, caches, SMP, 16 KB `pmap` attrs), `pexpert/device_tree.c` | ✅ **Reference** | Cross-check alongside Asahi Linux and m1n1 during **N2–N4** and **N8** (Samsung UART, AIC2/FIQ, `/chosen` seeds, SMP). Clean-room Go implementation |
| **Tier B: Design patterns** | `osfmk/ipc/` (Mach ports, rights, dead-name notifications), `osfmk/kern/ledger.c` (resource ledgers), `iokit/` (`IORegistry` power plane, `IOMapper` scatter-gather) | ✅ **Adapt ideas** | Inform **Q6** (`kernel/ipc` ports + per-`Proc` resource quotas) and **N5** (`drivers/apple/dart` scatter-gather IOMMU & PMGR power ordering) |
| **Tier C: Do not port** | Mach `vm_map`/`pmap` EL0 isolation, Clutch scheduler, MIG IDL, IOKit C++ `OSObject`/`OSMetaClass`, `bsd/netinet` (`mbuf`), `bsd/vfs` (`vnode`/UBC), Mach-O/Darwin syscall ABI | ❌ **Skip** | Conflicts with godeep's Go-native single-address-space model, duplicates superior Go packages (gVisor, `io/fs`, `wazero`), and would take years without delivering any M1 peripheral drivers |

---

## 5. When *would* porting XNU make sense?

Porting XNU (or targeting the Darwin/Mach-O ABI) only becomes the right starting point if the project's **primary goal changes**:

| Goal | Right foundation |
|---|---|
| A Go-native OS where Go's concurrency model *is* the OS, running fast on QEMU + Apple Silicon M1 Pro | **Current `godeep` (TamaGo + Go core + Asahi/m1n1 driver specs + selective XNU CPU/IPC ideas)** |
| Running unmodified macOS Mach-O binaries / `dyld` / Cocoa apps on bare metal | **A Darwin-ABI kernel** (EL0/EL1 hardware isolation, 16 KB `vm_map`/`pmap`, Mach-O loader, BSD+Mach syscalls — closer to Darling or a bare-metal rewrite of XNU in C/Rust/unsafe-Go, not TamaGo) |
| Running Linux ELF binaries in Go | **gVisor Sentry** (as noted in [`ReUsingGVisor.md`](ReUsingGVisor.md) §4) |

If you ever *did* pivot to an XNU-in-Go kernel for macOS binary compatibility, the pieces of `godeep` that would still carry over are:
- `board/applem1` early boot (`entry_arm64.s`, self-relocation, EL2 VHE drop, early framebuffer/fault screen) and `tools/fakem1n1` / `tools/m1n1boot` / `tools/godeep-partition.sh`;
- `kernel/pgtable` (extended to 16 KB granules and per-task TTBR0 tables);
- `kernel/mmio`, `kernel/dma`, `kernel/rtkit`, and every Apple hardware driver (`drivers/apple/*`).

Everything above that (`kernel/proc`, `kernel/vfs`, `programs/sh`, `programs/wasm`, and TamaGo's single-address-space runtime assumptions) would be replaced.

---

## 6. Bottom line for godeep's roadmap

You made the right call starting `godeep` the way you did:
1. **Keep the current architecture** — TamaGo single-address-space Go kernel, `board/virt` + `board/applem1`, `kernel/fdt`, `drivers/*` host-tested with `mmio.Fake`, gVisor `net/netstack`, and `wazero` WASI.
2. **Use Asahi Linux + m1n1 as the primary reference for M1 peripherals** (Track 2 milestones **N3–N8** in [`TargetMyMac.md`](TargetMyMac.md)), because open-source XNU does not contain those drivers.
3. **Keep a local checkout of `apple-oss-distributions/xnu` handy as a secondary reference** for:
   - Apple Silicon CPU/UART/FIQ/SMP quirks (`osfmk/arm64/`, `pexpert/`), and
   - Capability IPC (`osfmk/ipc/`) and resource ledgers (`osfmk/kern/ledger.c`) when designing **Q6**.
