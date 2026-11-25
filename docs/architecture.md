# godeep: a Go-native kernel and OS

## Context
`~/Documents/godeep` is empty. The goal is an OS where Go's concurrency model *is* the process model: there are no separate address spaces and no OS context switches. A "process" is a supervised group of goroutines, IPC goes over channels, the Go runtime scheduler is the OS scheduler, and memory safety comes from the language, not the MMU.

Decisions made:
- **Isolation:** Go only, single address space (Singularity-style language isolation).
- **Foundation:** TamaGo (`GOOS=tamago`, bare-metal Go runtime). Target `amd64` on the QEMU `microvm` board first.

The first deliverable is `CLAUDE.md` plus the repo skeleton. After that, work proceeds in phases, each ending with something that boots.

## Core architecture (the "concurrency layer")
| Classic OS concept | godeep equivalent |
|---|---|
| Process | `proc.Proc`: a PID, a root `context.Context`, a goroutine group (errgroup-style), a capability table, stdio channels and accounting |
| fork/exec | `proc.Spawn(ctx, prog, args, caps)` runs `go prog.Main(p)` |
| kill / signals | cancel the context, plus a `Signals <-chan Signal`. Kills are cooperative: Go cannot preempt-kill a goroutine. Every kernel API blocks on `ctx`, so a cancelled process unwinds at its next kernel call. |
| Scheduler | Go runtime (GMP). The kernel adds policy: priorities through admission limits and semaphores, plus per-process goroutine and CPU-time accounting. |
| Syscalls | Plain Go method calls on capability interfaces (`cap.File`, `cap.Net`, `cap.Spawner`…). There is no trap. |
| IPC / pipes | Typed channels. `ipc.Port` adds named services with request/reply. A shell pipe is a `chan []byte`. |
| File descriptors | Capability handles: unforgeable Go values that are passed in and never looked up globally |
| Drivers | One goroutine owns each device. IRQs become channel sends, and clients talk to the driver over channels. |
| Loadable programs | Phase 1: programs are compiled into the image and registered via `init()`. Phase 5: WebAssembly through **wazero** (pure Go), which gives dynamic loading with sandboxing that is still goroutine-backed. |

There are two build targets from the same core packages:
- **tamago:** the real bare-metal kernel under QEMU.
- **hosted:** the same kernel running as an ordinary Linux/macOS process. Board drivers are swapped for host shims. This target is used for fast `go test` and development.

The two are separated by build tags (`//go:build tamago` vs `!tamago`).

## Repo layout (to create)
```
CLAUDE.md
go.mod                      module github.com/<user>/godeep
Makefile                    build / qemu / hosted / test targets
cmd/godeep/                 kernel main (tamago)
cmd/godeep-hosted/          same kernel as a host process
kernel/proc/                Proc, Spawn, supervisor tree, PID table
kernel/cap/                 capability interfaces + handle table
kernel/ipc/                 Port, request/reply, pipes
kernel/vfs/                 VFS interface, memfs, devfs, procfs
kernel/dev/                 driver model: Device, IRQ→chan bridge
kernel/klog/                kernel log ring buffer
board/microvm/              TamaGo board glue: console, timer, virtio
board/hosted/               host shims (stdin/stdout, os files)
programs/{init,sh,ps,cat,echo,ls,top}/
docs/architecture.md
```

## Phases
1. **Boot and skeleton.** Install the TamaGo toolchain, then build and boot a "hello" on QEMU microvm over a serial console. Base it on `usbarmory/tamago-example` and `tamago/board/qemu/microvm`, and check the current build/ldflags in the TamaGo README. Add the hosted target too. Write CLAUDE.md.
2. **The concurrency layer:** `proc`, `cap`, `ipc` and `klog`. PID 1 `init` supervises children and restarts them according to policy. Unit tests run on the host, including leak checks via `go.uber.org/goleak`.
3. **Userland:** `memfs` + `devfs` + `procfs` (`/proc/<pid>` shows goroutine count, state and CPU time). A shell with channel pipelines (`ls | cat`), `ps`, `kill` and `top`.
4. **Devices:** virtio-mmio console and block devices, and IRQ delivery as channel sends (polling is the fallback if the TamaGo amd64 IRQ path is limited). Networking uses **gVisor netstack** (pure Go, already used by tamago-example) over virtio-net, exposed as a `cap.Net`.
5. **Dynamic programs:** a wazero-based loader where each WASM module runs as a Proc and gets WASI calls mapped onto its capabilities. Memory limits come from the wasm memory cap.
6. **Hardening:** per-proc resource quotas, panic containment (each proc's goroutines get `recover` wrappers so a panic kills the proc, not the kernel), and a SMP/GOMAXPROCS check.

## CLAUDE.md contents
- Project vision and the concept-mapping table above (kept brief)
- Build commands: `make test` (host `go test ./...`), `make hosted`, `make kernel` (tamago), `make qemu`
- Rules:
  - Kernel APIs take `context.Context` first and must return on cancel.
  - No global mutable namespaces: authority comes only from capabilities.
  - Every goroutine is started through `proc.Go`, never a bare `go`, so it is accounted and its panics are contained.
  - Board-specific code lives only in `board/*` behind build tags.
  - Core packages must build on the host.
- Known constraints: cooperative kill, no `plugin`/cgo on tamago, single address space trust model

## Verification
- `go test ./...` on the host: proc spawn/kill/supervision, IPC, goroutine-leak tests.
- `make hosted && ./godeep-hosted` boots to the `godeep$` shell.
- `make qemu` boots TamaGo on microvm to the same shell over serial. There, `ps`, `echo hi | cat`, `kill <pid>` (the target proc's goroutines disappear from `/proc`) and `cat /proc/1/status` should all work.
- Phase 4: `wget`/ping through netstack with QEMU user networking.

Immediate next step after approval: create CLAUDE.md, go.mod, the Makefile and the Phase 1 skeleton, then get "hello" booting in QEMU.
