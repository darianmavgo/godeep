# godeep

godeep is a kernel and OS written in Go where **Go's concurrency model is the process model**. It has no separate address spaces and no OS context switches:

| Classic OS | godeep |
|---|---|
| Process | `proc.Proc`: PID, root `context.Context`, a supervised goroutine group, capability table, stdio channels, accounting |
| fork/exec | `proc.Spawn(ctx, prog, args, caps)` |
| kill / signals | context cancel + `Signals` channel (cooperative, see Constraints) |
| Scheduler | the Go runtime scheduler (GMP); the kernel only adds policy and accounting |
| Syscalls | ordinary method calls on capability interfaces (`cap.File`, `cap.Net`, …) |
| IPC / pipes | typed channels; `ipc.Port` for named request/reply services |
| File descriptors | capability handles (unforgeable Go values that are passed in) |
| Drivers | one goroutine owns each device; IRQs are delivered as channel sends |
| Programs | compiled into the image and registered via `init()`; later, WASM via wazero |

Isolation comes from the language: everything runs in one address space, and Go's memory safety stands in for the MMU. The kernel runs bare-metal on **TamaGo** (`GOOS=tamago`), with QEMU `microvm` on amd64 as the first target. The same core also builds as a **hosted** binary for macOS/Linux, which is used for fast development and tests.

Full roadmap: `docs/architecture.md`.

## Toolchain setup (one time)

- Host Go: `brew install go`
- TamaGo Go (no macOS prebuilt, so build it from source with the host Go). Keep the tag matching the `github.com/usbarmory/tamago` version in go.mod:
  `git clone --depth 1 --branch tamago-go1.27.4 https://github.com/usbarmory/tamago-go ~/tamago-go && (cd ~/tamago-go/src && ./make.bash)`
- QEMU: `brew install qemu`

## Build

```
make test      # go test ./... on the host (core packages must stay host-buildable)
make hosted    # build ./bin/godeep-hosted and run the kernel as a normal process
make kernel    # build bin/godeep.elf with the TamaGo toolchain (TAMAGO=path/to/tamago-go/bin/go)
make qemu      # boot bin/godeep.elf on qemu-system-x86_64 -M microvm, interactive shell on serial
```

In QEMU, ^C goes to godeep (it kills the foreground program). Quit QEMU with `Ctrl-A` then `X`, or type `halt` (the guest triple-faults and `-no-reboot` exits).

## Layout

- `cmd/godeep/`: bare-metal entry point (`//go:build tamago`)
- `cmd/godeep-hosted/`: host entry point
- `kernel/…`: portable core. **No board imports.**
  - `proc`: process table. Each Proc tags its goroutines with pprof labels `pid`/`proc`; children inherit them. `Snapshot()` parses `runtime.Stack(all)`, which in Go 1.27 prints labels in each goroutine header, to attribute every goroutine to a PID (this is what `ps` uses). `Kthread` = kernel goroutine (pid 0).
  - `tty`: line discipline, run as its own kernel goroutine: echo, editing, typeahead, ^C → `Interrupts()` channel
  - `prog`: registry of built-in programs (registered from `init()`, read-only after boot)
  - planned: cap, ipc, vfs, dev, klog
- `board/microvm/`, `board/hosted/`: the only places with platform code, behind build tags
- `programs/…`: `initd` (PID 1, respawns sh), `sh` (shell + builtins `ps`, `kill`, `mem`…), `coreutils` (demo programs)

## Rules

- Every kernel API takes `context.Context` first and **must return promptly when it is cancelled**. This is what makes kill work.
- Never use a bare `go` statement in kernel or program code. Start goroutines with `p.Go(fn)` so they are accounted to a Proc and their panics are contained (a panic kills the proc, not the kernel).
- There are no global mutable namespaces. Authority comes only from capabilities passed to a Proc.
- Prefer channels and ownership (one goroutine owns the state) over shared memory with mutexes.
- Platform-specific code lives only in `board/*`, behind `//go:build tamago` / `!tamago`.
- New core code needs host unit tests. Use `go.uber.org/goleak` for anything that spawns goroutines.

## Constraints to remember

- **QEMU needs `-m 3G` or more.** The microvm board maps RAM up to `0xb0000000`. With less memory, the stack lands in unbacked RAM and the guest silently triple-faults (jump to RIP=0).
- macOS has no KVM, so QEMU runs under TCG (`-cpu max`). tamago detects TCG and calibrates the TSC via the LAPIC timer. Don't use `kvmclock`/`-enable-kvm` flags there.
- Don't build with the `linkhwinit0` tag (tamago-example uses it): it removes the default `Hwinit0`, and the link fails unless we provide our own.
- To debug boot crashes, add `-d int,cpu_reset` to QEMU and look for `Triple fault` and the RIP/SP before it.

- **Kill is cooperative.** Go cannot preempt-kill a goroutine. A cancelled Proc dies at its next blocking kernel call.
- TamaGo has no cgo, no `plugin` and no `os/exec`. Anything dynamic goes through the (planned) wazero loader.
- Single address space means trust: only memory-safe Go (no `unsafe` outside `board/*` and `kernel/dev`).
