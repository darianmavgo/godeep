# Processes vs goroutines in godeep

godeep has no address spaces, no OS threads of its own and no context switches. A **process** (`proc.Proc`) is a bookkeeping and lifetime boundary drawn around a group of **goroutines**. This doc describes what each level of the hierarchy can do, and what it can't, as the code stands (`kernel/proc`, `programs/sh`, `programs/initd`, `kernel/dev`).

```
kernel (Boot ctx)
├── pid 0 goroutines: Kthread (irq, uart0, tty, …): no Proc
├── PID n  devmgr ──► PID m  drv/<device>        (ppid = devmgr)
├── PID n  netd, logd, confd, inputd, ntpd       (ppid 0)
└── PID 1  init ──► sshd ──► sh (one per SSH session) ──► pipeline stages
               └──► sh (console) ──► pipeline stages, background jobs
                      every Proc = main goroutine + p.Go goroutines (+ untracked `go`)
                      every goroutine runs on one of the Go runtime's Ms (one per core)
```

## 1. Processes (`proc.Proc`)

A Proc is created by `Table.Spawn(parentCtx, ppid, Spec)` (`kernel/proc/proc.go`). `Spec` gives it a name, args, stdin, stdout, a `Main` function, pipe ends to close when it dies (`Ends`) and `Limits`.

**What a process has**

| Field / method | What it is |
|---|---|
| `PID`, `PPID` | numbers from the process table; PIDs are never reused within a boot |
| `Name`, `Args`, `Started` | shown by `ps`, `top`, `proc:/PID/status` and `proc:/PID/cmdline` |
| `Context()` | the root `context.Context`. Every goroutine of the Proc gets it, and cancelling it is how the Proc is killed |
| `Stdin`, `Stdout` | an `io.Reader` and an `io.Writer`: the terminal, an `ipc.Pipe`, a file, or nothing (stdin then returns EOF). Bytes are counted (`IO()`) |
| `Limits` | `Time` (wall-clock lifetime), `Goroutines` (tracked goroutines), `Output` (stdout bytes), `Memory` (WASM linear memory) |
| `Go(fn)` | start another tracked goroutine in the Proc (§3) |
| `Kill()`, `Wait(ctx)`, `Done()`, `Err()`, `State()` | stop it, wait for it, read its exit error (main's return value, `ErrKilled`, `ErrLimit`, or `panic: …`), and its state: `running`, `exiting` (cancelled, goroutines still unwinding) or `dead` |

**Where its authority comes from.** The `Proc` struct holds no capabilities itself. A program's authority is whatever its `Main` closure was given. For the shell and everything it runs, that is `sh.Env` (`programs/sh/sh.go`):

| Capability in `sh.Env` | Grants |
|---|---|
| `FS *vfs.NS` | the volumes it can see (`ram:`, disks, partitions, `host:`, `proc:`, `sys:`), each read-only or writable |
| `Net cap.Net` | `Listen`/`Dial` (or dial-only, or none) |
| `Ports *ipc.Namespace` | the named request/reply services it can find and offer |
| `Dev`, `IRQ`, `DMA` | device authority: the device manager, the interrupt bridge, DMA memory |
| `Table` | the process table: `ps`, `kill`, spawning children |
| `TTY`, `Log`, `Clock`, `WASM` | the terminal, the kernel log, the wall clock, the compiled-module cache |
| `Halt`, `Kexec`, `NoPower` | power-off, reboot and kexec, or their refusal |

Nothing is global: a program that wasn't handed a capability has no way to name it. `restrict` builds a narrower `Env` (`vfs.NS.View`, `cap.Restricted`, `ipc.Namespace.Only`, `-nodev`, `-ro`) before spawning, and SSH sessions get a dial-only network.

**What a process can do**
- run any number of goroutines (up to `Limits.Goroutines`), all killed together;
- read and write its stdio, and use the capabilities it was given;
- spawn children, if it was given the `Table` and a reason to (the shell, `initd`, `sshd`, `devmgr`);
- contain panics: a panic in its main goroutine or a `p.Go` goroutine kills the Proc, not the kernel.

**What it can't do (the differences from Unix)**
- **No memory isolation.** Every Proc shares one address space. Go's memory safety stands in for the MMU, and `unsafe` is only allowed in `board/*`, `kernel/dev`, `kernel/mmio` and `kernel/dma`.
- **No preemptive kill.** `Kill` cancels the context. A goroutine stops at its next blocking kernel call or `ctx` check, so a goroutine spinning without either never dies. `Table.Shutdown` reports such Procs as stragglers.
- **No signals besides kill.** The process model in `CLAUDE.md` mentions a `Signals` channel, but `Proc` has none today. ^C reaches the shell through `tty.Interrupts()`, and the shell kills the foreground job.
- **No CPU or heap quota.** Memory is limited only for WASM linear memory. `top` *samples* goroutine states per Proc; there's no CPU-time accounting (TamaGo has no profiling timer).
- **No per-process file descriptors, environment or cwd** at the `proc` level. Those live in the program: the shell keeps variables in `sh.State`, and WASM programs get env/dirs from `programs/wasm`.

**When a process ends.** As with Go's `main`: when `Main` returns, the Proc's context is cancelled, even if other goroutines are still running. The Proc is `exiting` until every *tracked* goroutine has returned, then it's removed from the table and `Done()` closes. `Spec.Ends` (pipe ends, files) are closed as soon as the context ends, which wakes the process at the other end of a pipe.

## 2. Parent processes

A parent relationship has two independent parts, and they don't always line up:

| Link | Set by | What it does |
|---|---|---|
| **Lifetime**: the child's context derives from `parentCtx` | the first argument to `Spawn` | when the parent's context is cancelled (killed, or its main returned), every child's context is cancelled too, and so on down the tree |
| **PPID** | the `ppid` argument | display (`ps`), and **limit inheritance**: the child's `Limits` are the parent's narrowed by its own `Spec.Limits` (`Limits.Narrow`), so a child can never loosen them |

Who spawns whom today:

| Parent | Children | Lifetime ctx | PPID |
|---|---|---|---|
| kernel (`kernel.Boot`) | `init`, `devmgr`, `netd`, `logd`, `confd`, `inputd`, `ntpd`, autorun `sh -c` | the boot context | 0 |
| `devmgr` | `drv/<device>`, one per driver; restarted on crash | devmgr's | devmgr's PID |
| `init` (PID 1) | `sshd`; the console `sh` (respawned when it exits) | init's | 1 |
| `sshd` | one `sh` per session (killed when the client disconnects) | the session's | sshd's PID |
| `sh` | one Proc per pipeline stage, background job (`&`), or limited builtin | the shell's | the shell's PID |

Consequences:
- **No orphans and no reparenting.** When a parent dies, its children are cancelled with it, and their children with them. A background job (`a &`) belongs to the shell that started it and dies when that shell exits.
- **Killing a child doesn't touch the parent.** The parent learns about it through `Wait` or `Done`, as the shell does for `$?`, `wait` and `fg`.
- **PID 0 has no Proc.** Kernel services are spawned with `ppid 0`: they inherit no limits, and their lifetime is the boot context, cancelled by `halt` or the power button (`Table.Shutdown`: every Proc killed, 2 s grace).
- **A parent sees nothing of its child except what it kept:** the `*Proc` that `Spawn` returned, plus whatever pipes it wired up. There is no `waitpid(-1)`: a parent waits on specific children.

## 3. Threads

godeep has no threads of its own. "Thread" here means the Go runtime's **M**, which runs goroutines:

| | Hosted build (macOS/Linux) | TamaGo (QEMU, the Mac) |
|---|---|---|
| What an M is | a real OS thread | a CPU core: TamaGo hands an M to each core through `goos.Task` |
| How many | as the Go runtime decides; `GOMAXPROCS` Ps run Go code | one per core; `GOMAXPROCS` = cores started (`lsdev cpus`, `lsdev idle`) |
| Scheduling | the Go scheduler on top of the OS's | the Go scheduler alone. When nothing is runnable, the board's idle hook sleeps the core (`WFI` on `board/virt`, `WFE` on the Mac) |
| Preemption | async preemption by signals | **none**: a goroutine keeps its core until it blocks or allocates (CPU-bound loops must call `runtime.Gosched()`) |
| `runtime.LockOSThread` | pins the goroutine to its thread | a no-op: a goroutine can't be pinned to a core |

Threads don't belong to processes. Any M runs goroutines of any Proc, and the Go scheduler moves a goroutine between Ms freely. So a Proc has no thread count, no thread priority and no core affinity. The only per-core state godeep keeps is in the boards: idle counters (`lsdev cpus`), interrupt acknowledgement on the boot core, and per-core wake-ups (SGIs on QEMU, SEV on the Mac).

## 4. Goroutines

Every running thing in godeep is a goroutine. There are three kinds, and the difference matters:

| Kind | Started with | Counted in the Proc (`live`, `Limits.Goroutines`) | Proc's `Done` waits for it | Panic | Shown in `ps` under the Proc |
|---|---|---|---|---|---|
| **main** | `Table.Spawn` (runs `Spec.Main`) | yes | yes | kills the Proc | yes |
| **tracked** | `p.Go(fn)` | yes; `Go` returns false past the limit or once the Proc is dead | yes | kills the Proc | yes |
| **untracked** | a bare `go` statement: only inside libraries (gVisor, `x/crypto/ssh`, wazero); banned in kernel and program code | no | no | **stops the kernel** | yes (it inherits the labels) |
| **kernel** | `proc.Kthread(name, fn)` | no Proc at all | — | stops the kernel | as pid 0 |

**What a goroutine gets from its process**
- **The context.** `p.Go(fn)` calls `fn(p.Context())`, so the goroutine sees the kill as `ctx.Done()`. Every kernel API takes that context first and must return promptly when it's cancelled. That's the whole kill mechanism.
- **Its identity: pprof labels.** Every Proc goroutine carries `pid=<PID>` and `proc=<name>` from its first instruction (`goWithLabels`). Go copies a goroutine's labels to every goroutine it starts, so even untracked library goroutines are attributed to the right Proc. `proc.Snapshot()` parses `runtime.Stack(all)`, which prints labels in Go 1.27, to build `ps`, `top`, `proc:/PID/goroutines` and `pprof goroutine` output per PID.
- **Its capabilities,** but only by closure: a goroutine can use whatever the function it runs can reach, typically the program's `Env`.

**The parent process of a goroutine** is decided once, at birth:
- a goroutine started by `Spawn` or `p.Go` belongs to that Proc;
- a goroutine started with a bare `go` belongs to whichever Proc its creator belonged to (labels are inherited), but the Proc doesn't count it or wait for it;
- a `Kthread` goroutine, and the runtime's own goroutines (GC workers, timers; unlabelled, shown as PID -1 in `Snapshot`), belong to no Proc.

A goroutine can't move to another Proc, and a Proc can't adopt one. A goroutine that outlives its Proc is still running Go code with a cancelled context, attributed to a PID that `ps` no longer lists. That's why bare `go` is banned outside libraries, and why library code is only called from inside a Proc.

**What a goroutine can't do**
- be killed individually: you cancel the context it watches (its Proc's, or a narrower one derived from it);
- be preempted or pinned to a core on TamaGo (§3);
- hide from accounting: labels are inherited and can't be dropped by ordinary code (a goroutine could call `pprof.SetGoroutineLabels` itself, which is one more reason only trusted Go runs in the kernel's address space).

## 5. Side by side

| | Unix process | Unix thread | godeep Proc | goroutine |
|---|---|---|---|---|
| Isolation | own address space (MMU) | shares the process's | shares everything; Go's memory safety | shares everything |
| Identity | PID | TID | PID (+ pprof labels) | goroutine ID + its Proc's labels |
| Created by | `fork`/`exec` | `pthread_create` | `Table.Spawn` | `Spawn` (main), `p.Go`, bare `go` (libraries only) |
| Authority | uid, fds, namespaces | the process's | capabilities in its `Main` closure (`sh.Env`) | whatever its closure can reach |
| Killed by | signals, preemptively | with the process | context cancel, cooperatively | its Proc's context; never individually |
| Children when parent dies | reparented to init | — | cancelled with it | keep running until they notice the cancel |
| Crash | the process dies | the process dies | panic in a tracked goroutine kills the Proc | tracked: Proc dies; untracked: kernel stops |
| Scheduled by | the kernel | the kernel | (not scheduled itself) | the Go scheduler, on one M per core |
| Resource limits | `rlimit`, cgroups | — | `Limits`: time, goroutines, output, WASM memory | counted against its Proc if tracked |

See also: `docs/architecture.md` (the original design), `docs/DriverModel.md` (drivers as Procs under `devmgr`), `kernel/proc/proc.go`.
