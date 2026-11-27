# GPU support for godeep: options

> **Status (2026-11-26):** Option 1 (**`ramfb`**) is now implemented on `board/virt` (arm64, HVF default) via [`drivers/qemu/fwcfg`](../drivers/qemu/fwcfg) and [`drivers/simplefb`](../drivers/simplefb) (1280×800 framebuffer text console with 2000-line scrollback and gpm-style mouse pointer/select/paste; run `make qemu QEMU_DISPLAY=cocoa`), and the same `drivers/simplefb` console runs natively on the Apple M1 Pro's 3456×2234 display. Networking (gVisor `net/netstack` over `virtio-net` and `usb-net`) is also built, unblocking Option 6 (host offload). **Update (2026-11-27):** a native AGX driver for the Mac was started despite Option 8's verdict: `drivers/apple/agx`, ported from Asahi's drm/asahi, stage 1 of 5 built (see [`AGX.md`](AGX.md)). The sections below preserve the original design comparison (written when `amd64`/`microvm` was the default target).

This doc covers the realistic ways to give godeep a GPU: a screen to draw on, graphics, and GPU-style compute. It compares their cost and recommends an order to build them in. All of it was originally written against TamaGo v1.27.4 on QEMU `microvm`, x86-64 under TCG emulation on an Apple Silicon Mac, with Homebrew QEMU 11.1.

---

## TL;DR

| # | Option | What you get | Effort | Verdict |
|---|---|---|---|---|
| 1 | **ramfb** framebuffer | pixels in a window | small (days) | ✅ fastest way to see graphics |
| 2 | **virtio-gpu 2D** | framebuffer, resolution changes, damage flushes, HW cursor, multiple displays | medium (1–3 weeks) | ✅ **recommended display driver** |
| 3 | bochs-display / VGA (PCI) | plain linear framebuffer | small–medium | ⚠️ works, but needs PCI on microvm; worse than #2 |
| 4 | **CPU SIMD + goroutines as the "GPU"** | data-parallel compute and software rendering | small–medium | ✅ **recommended compute path**, very Go-native |
| 5 | Software rasterizer (on top of #1/#2) | 2D/3D drawing in pure Go | medium | ✅ pairs with #2 and #4 |
| 6 | **GPU offload to the host** (API remoting) | real GPU compute (Metal/CUDA) from godeep | medium | ✅ only practical route to real GPU power |
| 7 | virtio-gpu 3D (virgl / Venus / native context) | accelerated OpenGL/Vulkan through the host | very large | ❌ your QEMU has no virgl; it means writing a Mesa-class driver in Go |
| 8 | Real GPU passthrough (VFIO) + native driver | the actual hardware | enormous | ❌ impossible on macOS; on Linux+KVM it's a multi-year port of a C driver |

**Recommended path:** #2 for display, then #4 + #5 for rendering and compute inside godeep, then #6 when you need real GPU throughput.

---

## 1. Constraints that shape every option

- **No cgo, no dynamic libraries.** TamaGo builds pure Go images. You can't link Mesa, the CUDA runtime, Metal or vendor drivers. Every driver has to be written in Go.
- **One address space.** A GPU driver is just another kernel goroutine that owns a device. There is no user/kernel split to cross, which actually makes GPU command submission *simpler* than on Linux.
- **The host is a Mac.**
  - No KVM, so QEMU emulates an x86 CPU (TCG), and CPU-heavy work runs maybe 5–20× slower than native.
  - No VFIO, so a physical GPU can't be passed through.
  - Your Homebrew QEMU has **no `virtio-gpu-gl` / virgl device**. Checked: `qemu-system-x86_64 -device help` lists only `virtio-gpu-device`, `virtio-gpu-pci`, `ramfb`, `bochs-display`, `VGA` and similar.
- **What TamaGo already has.** `github.com/usbarmory/tamago/kvm/virtio` provides virtio 1.2 transports (MMIO, modern PCI, legacy PCI) and virtqueues. `soc/intel/pci` does PCI enumeration, and `dma` manages DMA-able memory. The virtio-net path uses this today. A GPU driver reuses all of it; you only write the GPU protocol.
- **Display output.** Today `make qemu` runs with `-display none`. Any graphics option needs `-display cocoa` (a native macOS window) to be visible.

---

## 2. The options in detail

### Option 1: ramfb (simplest possible framebuffer)

QEMU's `ramfb` device scans out a framebuffer that lives **in guest RAM**. You configure it once through QEMU's firmware-config interface (`fw_cfg`):
1. Allocate a buffer, e.g. 1024×768×4 bytes, from the `dma` region.
2. Write a 28-byte config record (address, pixel format `XR24`, width, height, stride; big-endian) to the `fw_cfg` file `etc/ramfb`.
3. Draw into the buffer. QEMU displays it continuously.

```sh
make qemu QEMU_FLAGS="... -device ramfb -display cocoa"
```

| Pros | Cons |
|---|---|
| About 150 lines of Go. No virtqueues, no interrupts. | No damage tracking: QEMU rescans the whole buffer. |
| Perfect for "hello pixels" and testing a renderer. | Fixed resolution, no cursor, single display. |
| | Must verify that microvm's `fw_cfg` exposes `etc/ramfb`; it's normally used with UEFI/q35. |

### Option 2: virtio-gpu 2D (recommended display driver)

On microvm, `-device virtio-gpu-device` shows up as a virtio-mmio device:
- The slots start at `0xfeb00000`, `0x200` apart. tamago's microvm board already hard-codes the net device at `+0x2e00`.
- Probe each slot's `DeviceID` register; **16 = GPU**.

The 2D protocol is small. All commands go on the *control* virtqueue:

```
GET_DISPLAY_INFO                         → which displays exist, preferred size
RESOURCE_CREATE_2D (id, B8G8R8X8, w, h)  → a host-side surface
RESOURCE_ATTACH_BACKING (id, guest pages)→ back it with our DMA memory
SET_SCANOUT (display 0, resource id)     → show it
per frame:
  TRANSFER_TO_HOST_2D (rect)             → copy the changed rectangle
  RESOURCE_FLUSH (rect)                  → present it
```

A second *cursor* virtqueue carries `UPDATE_CURSOR` / `MOVE_CURSOR`, which gives you a hardware mouse pointer. A config-change interrupt tells you when the window was resized (`EVENT_DISPLAY`).

```sh
make qemu QEMU_FLAGS="... -device virtio-gpu-device -display cocoa"
```

| Pros | Cons |
|---|---|
| Reuses `tamago/kvm/virtio` (`MMIO`, `VirtualQueue`), so only about 600–1000 lines of protocol code. | Still CPU rendering: the "GPU" only copies and displays your pixels. |
| Damage-based flushes (efficient), resizing, cursor, up to 16 displays. | Needs interrupt or polled completion handling on the virtqueue. |
| The **same driver** works on Linux/KVM, cloud VMs and arm64 `virt`. It's the standard paravirtual display. | |
| It's the foundation option 7 would build on later. | |

### Option 3: bochs-display / VGA over PCI

These are simple PCI devices: a framebuffer in a PCI BAR, configured through a few registers. microvm has no PCI by default; you'd run it with `-machine microvm,pcie=on` and enumerate the bus with `tamago/soc/intel/pci`.

It's a reasonable fallback, but virtio-gpu gives more (damage, cursor, resize) for similar effort, and it stays on the MMIO path godeep already uses. **Skip unless option 2 hits a wall.**

### Option 4: CPU SIMD + goroutines as the "GPU" (recommended compute path)

A GPU is a machine for running *the same small function over a huge grid of data in parallel*. godeep already has both halves of that in pure Go:
- **Wide lanes:** Go 1.27 ships the `simd` package (portable vectors) and `simd/archsimd` (AVX/AVX2/AVX-512 on amd64). godeep's Makefile already builds with `GOEXPERIMENT=simd`.
- **Many cores:** goroutines spread across all CPUs. Boot QEMU with `-smp N`; tamago brings up the extra cores (`InitSMP`), and `GOMAXPROCS` follows.

So a "compute shader" in godeep is just a Go function, and a "dispatch" is a goroutine fan-out:

```go
// kernel/gpu/compute.go (sketch)
// Dispatch runs kernel over a 1-D grid in workgroups, one goroutine each,
// like vkCmdDispatch. It is cancellable like every godeep kernel API.
func Dispatch(ctx context.Context, p *proc.Proc, n, group int, kernel func(lo, hi int)) error {
	var wg sync.WaitGroup
	for lo := 0; lo < n; lo += group {
		hi := min(lo+group, n)
		wg.Add(1)
		p.Go(func(ctx context.Context) { defer wg.Done(); kernel(lo, hi) })
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }() // fence
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

Inside `kernel`, use `simd.Float32s` loads, multiply-adds and stores to process 8–16 floats per instruction.

- **Pros:**
  - Zero drivers; works today.
  - Every workgroup shows up in `ps -g`.
  - Cancel and kill work.
  - The same code runs hosted at full native speed on your Mac.
- **Cons:**
  - It's still a CPU: roughly 10–100× short of a real GPU on big parallel jobs.
  - Under TCG the SIMD instructions are emulated, so they're slow. Real speed needs KVM (a Linux host) or an arm64 target with Mac virtualization (HVF).
  - Check that tamago enables the AVX state in XCR0 before `archsimd` uses wide vectors.

### Option 5: a software rasterizer in Go

Put pixels into option 1/2's framebuffer with pure-Go code. Rough sizes:

| Job | Size |
|---|---|
| 2D: fill rectangles, blit, alpha blend, text (`golang.org/x/image/font` + `x/image/vector`) | 500–1500 lines for a usable compositor plus widgets |
| 3D: a triangle rasterizer (edge functions, z-buffer, perspective-correct UVs) | ~1–2k lines; pure-Go examples exist (e.g. fogleman/fauxgl) to learn from |

Parallelize it with option 4: split the screen into tiles, give each tile to a goroutine (a binned tile renderer), and use SIMD for shading. This is how CPU GPU-emulators such as llvmpipe and SwiftShader work.

### Option 6: GPU offload to the host (API remoting)

This is the only realistic way to use your Mac's **actual** GPU from godeep. godeep doesn't drive the GPU; it sends work to a host program that does:

```
godeep process ──cap.GPU──▶ gpu-client goroutine
        │ serialize: buffers + kernel name + grid size
        ▼
   virtio-net + gVisor netstack TCP  (virtio-vsock would be ideal, but
   QEMU on macOS has no vhost-vsock device; it needs a Linux host)
        ▼
host daemon (normal macOS Go program, cgo + Metal / MPS, or CUDA on Linux)
        │ runs the kernel on the real GPU
        ▼
results back over the same channel → fence channel closes
```

- **Pros:**
  - Real GPU throughput for compute (ML inference, matrix multiplies, image filters).
  - The guest side stays pure Go and fits the capability model: a process can only use the GPU if it was handed a `cap.GPU`.
- **Cons:**
  - Data has to be copied across the VM boundary. Fine for big batch jobs, bad for per-frame graphics.
  - You need the network stack first (Phase 4 of the roadmap). vsock isn't available on macOS QEMU, so it's TCP over virtio-net there.
  - The host daemon is outside godeep.

This is the same idea as rCUDA, Android's gfxstream and the "API remoting" mode in libkrun.

### Option 7: virtio-gpu 3D (virgl / Venus / native context): not now

virtio-gpu's 3D mode makes the host GPU render for the guest. But:
1. **Host side:** QEMU must be built with virglrenderer (or rutabaga). Your Homebrew build isn't, and on macOS it would run on top of ANGLE/MoltenVK, which is fragile.
2. **Guest side:** the guest must produce **Gallium/TGSI command streams (virgl)** or **serialized Vulkan (Venus)**. That's the job Mesa's ~100k-line user-space driver does on Linux. In godeep you'd be writing a Go OpenGL or Vulkan implementation that emits those streams.

Revisit only if godeep moves to a Linux+KVM host *and* has a real need for accelerated 3D. Even then, a small subset (one fixed pipeline over Venus) is the sane scope.

### Option 8: real GPU passthrough + native driver: no

- **Passthrough:** this needs VFIO and an IOMMU, which means a Linux host. macOS can't do it, and Apple-silicon GPUs can't be passed through at all.
- **The driver:** even on Linux, godeep would need a full native driver:
  - **NVIDIA:** the open kernel modules plus GSP firmware. The firmware simplifies a lot, but it's still hundreds of thousands of lines of C to port.
  - **AMD amdgpu:** millions of lines.
  - **Intel i915/xe:** similar.
- **Plus a shader compiler**, since you have to generate GPU machine code.

This is a research-lab-scale project and shouldn't be on the roadmap.

---

## 3. How a GPU fits godeep's concurrency model

Whatever is underneath (virtio-gpu, the SIMD "GPU" or host offload), godeep should present it the same way, using the same patterns as the rest of the kernel.

| GPU concept | godeep design |
|---|---|
| driver | one kernel goroutine (`pid 0`, `gpu0`) owns the device and its virtqueues; nobody else touches the hardware |
| command buffer / queue | `chan Cmd` into the driver goroutine |
| fence / completion | a `chan struct{}` closed when the work is done; `select` on it next to `ctx.Done()` |
| GPU context per app | a capability (`cap.Display`, `cap.Surface`, `cap.GPU`) handed to a process; no global `/dev/dri` |
| vsync | a `time.Ticker` (or the device's flush completion) feeding a channel |
| compositor / window manager | an ordinary process that owns the scanout and receives surfaces from clients over channels |
| GPU hang / app crash | the process's context is cancelled; the driver drops its resources; `ps -g` shows the stuck goroutine |

A sketch of the interfaces:

```go
package gpu

// Surface is a drawable image owned by one process.
type Surface interface {
	Bounds() image.Rectangle
	Pix() []byte                                         // B8G8R8X8, stride = 4*width
	Present(ctx context.Context, damage image.Rectangle) error // TRANSFER + FLUSH; blocks on the fence
}

// Display is the capability to put surfaces on screen.
type Display interface {
	Modes(ctx context.Context) ([]image.Point, error)
	NewSurface(ctx context.Context, w, h int) (Surface, error)
	SetScanout(ctx context.Context, s Surface) error
	Events() <-chan Event // resize, hotplug
}

// Compute is the capability to run data-parallel kernels: SIMD+goroutines
// locally (option 4) or a host GPU (option 6) behind the same interface.
type Compute interface {
	Dispatch(ctx context.Context, n, group int, k Kernel) (Fence, error)
}

type Fence <-chan struct{}
```

With this boundary, programs written against `Display` and `Compute` don't change when the backend moves from ramfb to virtio-gpu, or from CPU SIMD to the host's Metal GPU.

---

## 4. Recommended roadmap

| Step | Deliverable | Demo |
|---|---|---|
| **G1** | `board/microvm` virtio-mmio probe + **virtio-gpu 2D** driver (`kernel/dev/virtiogpu`). Do ramfb first if you want pixels in a weekend. | `fill` program paints the screen a color; `ps -g` shows the `gpu0` kernel goroutine |
| **G2** | `gpu.Display`/`Surface` capabilities + a **compositor** process + 2D drawing (rects, blits, text) | `clock` program shows a live clock in a window next to a `term` program that mirrors the shell |
| **G3** | **SIMD + goroutine compute** (`gpu.Compute` local backend) and a tiled **software rasterizer** on top of it | spinning shaded 3D cube; `ps -g` shows one goroutine per tile |
| **G4** | **Host offload** backend for `gpu.Compute` over TCP (virtio-net + netstack; vsock on a Linux host), with a small macOS host daemon using Metal | the same `matmul` program runs locally (SIMD) or on the Mac's GPU by swapping the capability |
| later | Re-evaluate virtio-gpu 3D (Venus) only on a Linux+KVM host | |

Two changes help every step:
- **Done: arm64 is now the default target** (`board/virt`, QEMU `virt` + HVF), so CPU-side rendering and compute already run at native speed. On `virt`, virtio-mmio devices live at `0x0a000000` (32 slots, `0x200` apart, SPIs from INTID 48), and the GPU driver probes there the same way. Use NEON through `simd`/`simd/archsimd` on arm64.
- **Interrupt-driven virtqueue completion** (I/O APIC routing). The driver goroutine then sleeps until the GPU finishes, instead of polling.

---

## 5. References

- virtio spec v1.2, section 5.7 "GPU Device": the 2D/3D command set and virtqueues
- QEMU docs: `microvm` machine, `virtio-gpu`, `ramfb`
- TamaGo: `kvm/virtio` (transports and virtqueues), `soc/intel/pci`, `dma`, `board/qemu/microvm`
- Go `simd` and `simd/archsimd` packages (`GOEXPERIMENT=simd`, Go 1.26+)
- Mesa's virgl and Venus drivers, for a sense of what 3D over virtio involves
- gfxstream / libkrun API remoting, for prior art on GPU offload
