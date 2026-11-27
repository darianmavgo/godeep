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
| **1. Bring-up** | GPU identity (generation, variant, revision, clusters, core masks), MMU fault decoding, UAT page tables (map, unmap, lookup), the handoff lock and init, the kernel's tables in slot 0, a fault watch; `lsdev gpu0` | ✅ built 2026-11-27, host-tested (20× race); not yet run on the Mac | `go test ./drivers/apple/agx`; on the Mac: boot with `godeep.agx`, `lsdev gpu0` |
| **2. Firmware boot** | kernel allocators (private, shared, GPU-shared ranges with their protections), RTKit over UAT-mapped buffers, the channels, InitData for firmware compat 12.3 (and 13.5), the t600x I/O mappings, `Initialize` + `MSG_INIT`, the firmware log into `dmesg` | ⬜ next; the bulk of the work (`fw/initdata.rs` + `initdata.rs` alone are ~2,400 lines) | firmware log lines and device-control acknowledgements on the Mac |
| **3. Compute jobs** | GPU contexts (user VMs in slots 1–63, TLB invalidation), work queues, compute submission, events | ⬜ | a compute job runs and writes a buffer |
| **4. Shaders** | AGX ISA binaries: precompiled with Mesa's Asahi compiler on Omarchy and embedded, then a small Go assembler for compute kernels | ⬜ | a shader computes a known result |
| **5. Rendering** | vertex/fragment jobs, tiler and buffer manager, then a `gpu.Display` drawing into the framebuffer | ⬜ | `gpu test` and `wm` on the Mac, GPU-rendered |

Display mode setting, brightness and vsync belong to **DCP** (the display coprocessor), a separate driver. Until then, the screen stays m1n1's framebuffer (`drivers/simplefb`).

## Stage 1 in detail

- `id.go`: `ReadID` decodes the six ID registers and the core masks (a long bit string, `cores` bits per cluster); `T6000.Check` refuses any other GPU.
- `fault.go`: `DecodeFault` for G13's `FAULT_INFO` (address, slot, unit, level, reason).
- `uat.go`: `PageTable` over a window of physical memory (`mmio.Window{Base: 0}` on the Mac). It refuses to replace a mapping and to map beyond the 42-bit output size. Owned (user) tables get non-global entries; the kernel's table is borrowed from iBoot. New table pages come from the DMA region (`DMAPages`).
- `handoff.go`: the lock (byte flags next to the firmware's: `mmio.Window.Write8` is a compare-and-swap on the containing word, so the firmware's byte survives), `Init` (magic exchange, 1 s timeout), and `TTBs.SetKernel`.
- `fdt.go`: windows, mailbox, power domains, the three regions, and `apple,firmware-version` / `apple,firmware-compat`. A region m1n1 didn't fill in is an error that `lsdev` shows.
- `agx.go`: the driver. Power on → ID → firmware-compat check → start the coprocessor → handoff → slot 0 → `Ready` → poll the fault register once a second, counting changes only.

**On the Mac it's opt-in:** `board/applem1` names the node `gpu0` and leaves the driver off unless `/chosen/bootargs` has `godeep.agx` (`make native BOOTARGS=godeep.agx`), so it can't affect the SMP-hang bisect. Expected `lsdev gpu0` on success: `G13S …: 2 clusters × 8 cores, 16 active`, the firmware version, and "UAT handed off". Failure modes are reported, not fatal: power, an unpowered GPU (ID reads 0), unknown firmware compat, or "no answer from the GPU firmware".

## Open questions for stage 2

- **TLB invalidation.** Asahi invalidates the GPU's TLB with the CPU's own broadcast `TLBI` instructions (the UAT listens to them). That needs a small assembly helper in `board/applem1` behind an interface.
- **Cache coherency.** godeep maps DRAM outside its own region Normal non-cacheable, while the firmware maps some ranges cached ("private"). Check what Asahi does for cached, non-coherent ranges before relying on them.
- **Which firmware is installed.** `apple,firmware-compat` decides the InitData layout (12.3 or 13.5); the first `godeep.agx` boot shows it.
- **No QEMU rehearsal.** fakem1n1 has no GPU node, and a fake firmware convincing enough for stage 2 would be most of a GPU emulator. Stage 2 is host-tested against recorded structures, then tried on the Mac.
