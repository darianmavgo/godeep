# AGX: the M1 Pro's GPU

The plan and status for `drivers/apple/agx`, a native driver for this Mac's GPU (Apple AGX G13S, `apple,agx-t6000`, 2 clusters × 8 cores).

**Reference:** Asahi Linux's `drivers/gpu/drm/asahi` (Rust, about 17,700 lines, licensed "GPL-2.0-only OR MIT"; godeep uses it under MIT and says so in the package doc), Asahi's device trees (`t600x-die0.dtsi`, `t6000.dtsi`), and m1n1 (`src/kboot_gpu.c`, `proxyclient/m1n1/agx`). The open-source XNU has no GPU driver (`XNUPort.md`).

## How the GPU works

The kernel barely touches GPU registers. Almost everything goes through the **GPU firmware**, which iBoot loads and which runs on an ASC coprocessor speaking RTKit (the same protocol as the SSD and SMC):

1. **Power**: the `ps_gfx` domain (`drivers/apple/pmgr`).
2. **The coprocessor**: its CPU is started through the `asc` window (`drivers/apple/asc`).
3. **The UAT** (the GPU's MMU): ARM64-like page tables, 16 KB pages, 3 levels, 39-bit GPU addresses, 42-bit physical. iBoot prepares three regions, and m1n1 copies their addresses into the device tree:
   - `uat-handoff`: a lock shared by the kernel and the firmware (Peterson's algorithm), magic numbers, TLB-flush requests;
   - `uat-pagetables`: the kernel's top-level table (TTB1), with the firmware already mapped in it;
   - `uat-ttbs`: the slot table, 64 contexts × (TTB0, TTB1). Slot 0 is the kernel's; slots 1–63 are GPU contexts.
4. **Firmware init**: the kernel allocates memory in GPU kernel space (`0xffffffa000000000`…), builds a large, firmware-version-specific **InitData** structure (power and performance tables from the device tree, hardware constants, pointers to ring-buffer channels), boots RTKit, and sends the InitData address on the firmware endpoint.
5. **Work**: GPU contexts, work queues and jobs (vertex, fragment, compute) go through the channels; completions come back as events. The jobs carry **shader binaries** in the AGX instruction set.

## Stages

| Stage | What | Status | How it's checked |
|---|---|---|---|
| **1. Bring-up** | GPU identity (generation, variant, revision, clusters, core masks), MMU fault decoding, UAT page tables (map, unmap, lookup), the handoff lock and init, the kernel's tables in slot 0, a fault watch; `lsdev gpu0` | ✅ built 2026-11-27, host-tested (20× race); not yet run on the Mac | `go test ./drivers/apple/agx`; on the Mac: `lsdev gpu0` (on by default; `godeep.noagx` turns it off) |
| **2. Firmware boot** | kernel allocators, RTKit over UAT-mapped buffers, the channels, InitData for firmware 12.3 and 13.5, the t600x I/O mappings, `Initialize` + `MSG_INIT`, the firmware log | ✅ built 2026-11-27, host-tested end to end against a simulated firmware; not yet run on the Mac | on the Mac: `lsdev gpu0` says "firmware initialized" and shows firmware log lines; the calibration-blob comparison says "matches" |
| **3. Compute jobs** | GPU contexts (user VMs in slots 1–63, TLB invalidation), work queues, compute submission, events | ⬜ | a compute job runs and writes a buffer |
| **4. Shaders** | precompiled AGX binaries, embedded (below); later a small Go assembler for compute kernels | ⬜ (pipeline chosen 2026-11-27) | a shader computes a known result |
| **5. Rendering** | vertex/fragment jobs, tiler and buffer manager, then a `gpu.Display` drawing into the framebuffer | ⬜ | `gpu test` and `wm` on the Mac, GPU-rendered |

Display mode setting, brightness and vsync belong to **DCP** (the display coprocessor), a separate driver. Until then, the screen stays m1n1's framebuffer (`drivers/simplefb`).

## Stage 1 in detail

- `id.go`: `ReadID` decodes the six ID registers and the core masks (a long bit string, `cores` bits per cluster); `T6000.Check` refuses any other GPU.
- `fault.go`: `DecodeFault` for G13's `FAULT_INFO` (address, slot, unit, level, reason).
- `uat.go`: `PageTable` over a window of physical memory (`mmio.Window{Base: 0}` on the Mac). It refuses to replace a mapping and to map beyond the 42-bit output size. Owned (user) tables get non-global entries; the kernel's table is borrowed from iBoot. New table pages come from the DMA region (`DMAPages`).
- `handoff.go`: the lock (byte flags next to the firmware's: `mmio.Window.Write8` is a compare-and-swap on the containing word, so the firmware's byte survives), `Init` (magic exchange, 1 s timeout), and `TTBs.SetKernel`.
- `fdt.go`: windows, mailbox, power domains, the three regions, and `apple,firmware-version` / `apple,firmware-compat`. A region m1n1 didn't fill in is an error that `lsdev` shows.
- `agx.go`: the driver. Power on → ID → firmware-compat check → start the coprocessor → handoff → slot 0 → `Ready` → poll the fault register once a second, counting changes only.

**On the Mac it's on by default** (since 2026-11-27, so `make native` enables everything built): `board/applem1` names the node `gpu0` and runs the driver unless `/chosen/bootargs` has `godeep.noagx` (`make native BOOTARGS=godeep.noagx`), the way to rule it out while bisecting a boot problem. `godeep.agx`, the earlier opt-in, is still accepted and changes nothing. Expected `lsdev gpu0` on success: `G13S …: 2 clusters × 8 cores, 16 active`, the firmware version, and "UAT handed off". Failure modes are reported, not fatal: power, an unpowered GPU (ID reads 0), unknown firmware compat, or "no answer from the GPU firmware".

## Stage 2

- **Firmware structure layouts** (`drivers/apple/agx/fw`, `layout_gen.go`): generated by `tools/agxlayout` from drm/asahi's `fw/*.rs` for G13 with firmware 12.3 and 13.5 (92 structures each). The tool parses the Rust structs, evaluates each field's `#[ver(...)]` condition, and lays them out as rustc does (`repr(C)`, packed types: `U32`/`U64`/GPU pointers have alignment 1, which is easy to get wrong by hand). **Checked:** 566 of 576 fields named after their 12.3 offset (`unk_1cc`, `pad_644`) land there; the other 10 are reviewed naming quirks, listed in the tool, and any new mismatch fails it. `RuntimePointers` (0x6bc0) and `HwDataB` (0xb6c, an 0xb80 allocation) also match sizes in m1n1's notes. Regenerate: `go run ./tools/agxlayout -src …/drm/asahi/fw -out drivers/apple/agx/fw/layout_gen.go`. `fw.Writer` fills a structure by field name; writes to fields another version lacks are dropped, so one builder serves 12.3 and 13.5.
- **Kernel GPU memory** (`kalloc.go`): Asahi's ranges (private, GPU-RO, shared, shared-RO, GPU-shared, RTKit) in the kernel VM, backed by DMA-region pages. A bump allocator with guard pages that never frees: freeing safely needs the GPU TLB invalidation and firmware cache flush that aren't ported. **Every range is mapped uncached for the firmware**, unlike Linux: godeep's CPU writes DRAM through a non-cacheable mapping, which bypasses the coherency the firmware's cache relies on.
- **RTKit over the UAT** (`RTKitIOMMU`): RTKit's buffers are mapped into the kernel VM. `kernel/rtkit` now unmaps by IOVA (it passed the physical address, harmless with SART or no IOMMU, wrong here).
- **Channels** (`channel.go`): the ring buffers with their state blocks, receive (6 sub-channels for the firmware log) and transmit (waits when full, `WaitFor` the firmware to consume), and the full set the firmware expects (device control, fwctl, 4×3 pipes, event, log, ktrace, stats).
- **Power configuration** (`pwr.go`): the operating points (`operating-points-v2`: frequency, per-cluster voltage, power, filled in per device by m1n1) and ~45 `apple,*` power-loop properties, with drm/asahi's defaults. Floats are IEEE bits in the tree.
- **InitData** (`initdata.go`): a line-by-line port of drm/asahi's `initdata.rs` (HwDataA: power control loops; HwDataB: operating points, chip identity, the 20 I/O mappings of GPU registers the firmware drives itself; Globals; RuntimePointers: every channel, statistics blocks, the buffer-manager control block mapped at two addresses; FwStatus; the root with the UAT description). One builder serves 12.3 and 13.5 through the generated layouts. `fw.Writer` panics on a field name that no firmware version has, writes integers into float fields as floats (Rust's `.into()`), and refuses floats into integer fields, so a typo or a type slip fails the tests instead of producing a silently wrong structure.
- **Boot** (`agx.go`, `firmware.go`): after the stage-1 handoff the driver builds everything above, queues `Initialize` on the device-control channel (discriminant 0x19, 0x1a from 13.3), boots RTKit with its buffers in the kernel VM, starts endpoints 0x20/0x21, sends `MSG_INIT | InitData`, rings the device-control and kick doorbells, then drains the receive channels on every firmware doorbell; the firmware log goes to `lsdev gpu0` (last line; `GPU.Log()` keeps 64). On halt, RTKit is put to sleep.
- **Validation on the Mac:** drm/asahi compares its HwDataA, HwDataB and Globals with blobs m1n1 can provide (`hw-cal-a`, `hw-cal-b`, `globals` memory regions); godeep does the same and reports "matches" or the first differing offset in `lsdev gpu0`. If m1n1 provides them, that verifies the whole InitData port on the first boot.
- **Tests:** `TestInitData` builds InitData for both versions and checks values against drm/asahi's formulas (power loops, operating points, the I/O mappings and their page-table entries, pointers between structures). `TestDriver` runs the driver against a simulated firmware: handoff, RTKit handshake, `MSG_INIT`; the simulated firmware follows the InitData pointers through the page tables (as the real one does) to check the `Initialize` command and to write a log line, which must reach `lsdev`.

## Shaders: precompiled with Mesa

Mesa builds its own built-in GPU kernels (`src/asahi/libagx/*.cl`) this way: OpenCL C → SPIR-V (`mesa_clc`) → AGX binaries written out as C arrays (`asahi_clc INPUT.spv OUT.h OUT.c`, `src/asahi/clc`). For godeep:

1. On Omarchy, build Mesa with the Asahi driver and `-Dinstall-precomp-compiler=true` to get `mesa_clc` and `asahi_clc`.
2. Write compute kernels in OpenCL C, compile them with those two tools, and convert the arrays to `.bin` files (a small Go tool).
3. Embed them in `drivers/apple/agx` (`//go:embed`). Stage 3 submits them with the argument layout the precompiled kernels expect (libagx's conventions: to be read from `asahi_clc.c` and libagx before stage 3).

Nothing here runs until stage 3 can submit compute jobs.

## Open questions for stage 2

- **TLB invalidation.** Asahi invalidates the GPU's TLB with the CPU's own broadcast `TLBI` instructions (the UAT listens to them). That needs a small assembly helper in `board/applem1` behind an interface.
- **Cache coherency.** godeep maps DRAM outside its own region, and its DMA region, Normal non-cacheable. For now every kernel GPU range is mapped uncached for the firmware too (above). Mapping the DMA region cacheable (and coherent with the ASC) would allow Asahi's cached private ranges, but changes every other driver's assumptions.
- **Which firmware is installed.** `apple,firmware-compat` decides the InitData layout (12.3 or 13.5); the first `godeep.agx` boot shows it.
- **No QEMU rehearsal.** fakem1n1 has no GPU node, and a fake firmware convincing enough for stage 2 would be most of a GPU emulator. Stage 2 is host-tested against recorded structures, then tried on the Mac.
