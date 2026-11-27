# Processes, threads and goroutines

How Linux and macOS model processes and threads, and how Go's goroutines differ from both. This doc is about the general concepts only.

```
Linux / macOS kernel
└── PID 1 (systemd / launchd)
    └── process: address space, credentials, file descriptors, signal handlers, …
        ├── thread (kernel-scheduled): registers, stack, signal mask, TLS
        ├── thread
        └── child process (fork / posix_spawn) …

Go program (one process)
└── Go runtime: M OS threads, P = GOMAXPROCS processors
    └── goroutines (G), many per thread, scheduled by the runtime, not the kernel
```

## 1. Processes

A process is the operating system's unit of **isolation and ownership**: it owns an address space and resources, and it's what permissions are checked against.

### What a process has

| | Linux | macOS (XNU) |
|---|---|---|
| Kernel object | a *thread group* of `task_struct`s sharing one `mm_struct` (address space) | two halves: a **Mach task** (address space `vm_map`, Mach port namespace, threads) and a **BSD `proc`** (PID, credentials, file descriptors, signals) |
| Identity | PID (= the thread group ID, TGID) | PID (BSD side); the Mach task is reached through a task port |
| Address space | private, virtual, enforced by the MMU | the same |
| Credentials | real/effective/saved uid and gid, supplementary groups, **capabilities(7)** (root's powers split into `CAP_NET_ADMIN`, `CAP_SYS_ADMIN`, …) | uid/gid, plus **code signing** and **entitlements**, the **sandbox** (Seatbelt profiles), and **TCC** (user consent for camera, files, …) |
| Resources | file descriptor table, cwd and root, umask, environment, `rlimit`s | the same, plus **Mach port rights** (IPC endpoints) |
| Isolation and limits | **namespaces** (pid, mount, net, uts, ipc, user, cgroup, time) and **cgroups** (CPU, memory, I/O, pids): the basis of containers | no namespaces or cgroups; sandbox profiles, `rlimit`s, and system-wide memory pressure handling. Energy policy (App Nap, QoS) throttles background apps |
| Syscall filtering | seccomp-bpf | the sandbox |

### Creating a process

- **`fork()`** duplicates the calling process: the child gets a copy-on-write copy of the address space and copies of the fd table, signal dispositions, environment, cwd, credentials and limits. Only the thread that called `fork` exists in the child.
- **`exec*()`** replaces the program in the current process: a new address space, signal handlers reset to default, fds marked close-on-exec closed, everything else (PID, other fds, credentials unless setuid, cwd) kept.
- **`posix_spawn()`** does fork+exec in one call. On macOS it's the preferred way, because `fork` in a process using Apple's frameworks is unsafe (other threads' locks are copied in a held state). Linux implements it with `vfork`/`clone`.
- **Linux `clone()` / `clone3()`** is the general primitive: flags choose what parent and child share (memory, fds, signal handlers, namespaces). `fork`, `vfork` and thread creation are all `clone` with different flags.

### Ending a process

A process ends when it calls `exit` (or returns from `main`), or when a signal kills it. Its exit status is kept until the parent collects it. Until then the process is a **zombie**: it holds no memory, just a process-table entry.

Killing is **preemptive**: `SIGKILL` (and other fatal signals) stop the process wherever it is, and the kernel reclaims everything it owned: memory, fds, locks.

## 2. Parent processes

Every process except PID 1 has a parent: the process that created it (`getppid()`).

**What the parent relationship gives**
- **Exit notification and status.** The parent gets `SIGCHLD` when a child exits or stops, and collects the status with `wait`/`waitpid`/`waitid` (Linux also has `pidfd`s; macOS has kqueue's `EVFILT_PROC` with `NOTE_EXIT`).
- **Inheritance at creation.** A child starts with copies of the parent's fds, environment, cwd, credentials, limits, signal dispositions, namespaces (Linux) and sandbox (macOS). *Not* inherited: the PID, pending signals, `fcntl` record locks, timers, and (on Linux) memory locks.
- **Process groups and sessions.** A shell puts each job in its own process group; the terminal's foreground group gets ^C (`SIGINT`) and ^Z (`SIGTSTP`). A session ties process groups to a controlling terminal, and the terminal hanging up sends `SIGHUP`.

**What it doesn't give**
- **A child doesn't die with its parent.** It becomes an **orphan** and is reparented to PID 1 (`systemd`/`init` on Linux, `launchd` on macOS), or on Linux to the nearest **subreaper** (`PR_SET_CHILD_SUBREAPER`, used by service managers and container runtimes). To make a child die with its parent: Linux has `PR_SET_PDEATHSIG`; on macOS the child must watch the parent itself (kqueue on the parent's PID, or a pipe that closes).
- **No shared memory or authority after creation.** Parent and child are fully separate; anything they share afterwards (pipes, sockets, shared memory, Mach ports) has to be set up explicitly.
- **No control besides signals.** A parent can signal a child it has permission to signal (same user, or privileged), as any other process could.

If a parent never waits, its exited children stay zombies until the parent exits and PID 1 collects them.

## 3. Threads

A thread is the unit the kernel **schedules**: a register set and a stack, running inside a process and sharing its address space and resources.

### What threads share and what's their own

| Shared by all threads of a process | Per thread |
|---|---|
| address space (heap, globals, code, other threads' stacks) | registers, program counter, stack |
| file descriptors, cwd, umask | thread ID (Linux `gettid()`, macOS `pthread_threadid_np()`) |
| signal *handlers* | signal *mask*, pending thread-directed signals |
| PID, credentials (POSIX semantics) | thread-local storage, `errno` |
| resource limits | scheduling priority / QoS, CPU affinity (Linux) |

Because memory is shared, threads communicate directly and need synchronization (mutexes, condition variables, atomics). A bug in one thread (a wild write, an unhandled crash signal) takes down the whole process.

### Linux

- **1:1 threads (NPTL):** each `pthread` is a kernel task created by `clone(CLONE_VM | CLONE_FS | CLONE_FILES | CLONE_SIGHAND | CLONE_THREAD | …)`. To the kernel, a thread and a process are the same kind of object; they differ only in what they share.
- **Scheduler:** EEVDF (since kernel 6.6, replacing CFS) for normal threads, plus real-time policies (`SCHED_FIFO`, `SCHED_RR`, `SCHED_DEADLINE`). `nice` values and CPU affinity (`sched_setaffinity`) apply **per thread**.
- **Signals:** a process-directed signal goes to any one thread that isn't blocking it; `tgkill`/`pthread_kill` targets one thread.
- **Stacks:** 8 MB by default (from `ulimit -s`), allocated lazily.
- The process ends when its last thread exits, or when any thread calls `exit` (`exit_group`), which kills all of them.

### macOS

- **Mach threads:** every thread belongs to a Mach task; `pthread`s are 1:1 on Mach threads.
- **Scheduling by QoS** instead of raw priorities: *user-interactive*, *user-initiated*, *utility*, *background*. On Apple Silicon, QoS decides whether a thread runs on performance or efficiency cores. There's no CPU affinity API that pins a thread to a core.
- **Grand Central Dispatch (GCD):** most macOS code doesn't create threads directly but submits work to dispatch queues; the kernel's *workqueue* grows and shrinks a pool of threads for them.
- **Stacks:** 8 MB for the main thread, 512 KB by default for secondary threads.
- Signals follow POSIX like Linux; Mach exceptions are delivered to exception ports first (that's how debuggers and crash reporters see crashes).

### Cost

Threads are much cheaper than processes (no new address space), but each still costs a kernel object, a stack of hundreds of KB to MB, and a **kernel context switch** (a trip into the kernel, saving and restoring registers, cache and TLB effects) every time the scheduler moves between them. Thousands of threads are practical; millions aren't.

## 4. Goroutines

A goroutine is a function running concurrently, scheduled by the **Go runtime** inside one process, not by the kernel. `go f(x)` starts one.

### How they run: the G-M-P model

| Piece | What it is |
|---|---|
| **G** | a goroutine: a stack (starts at a few KB and grows and shrinks by copying), its saved registers and status |
| **M** | an OS thread (a `clone`d task on Linux; a `pthread` from libSystem on macOS, since Go calls the system's libraries there) |
| **P** | a processor: the right to run Go code, with a local run queue of Gs. There are `GOMAXPROCS` Ps (default: the number of CPUs; on Linux it also follows the cgroup CPU limit since Go 1.25) |

The runtime multiplexes many Gs onto a few Ms (**M:N scheduling**):
- An M needs a P to run Go code. Each P has its own run queue, and an idle P **steals** work from the others.
- A goroutine blocked on a channel, mutex, timer or network I/O is parked by the runtime and the M runs another G. No kernel context switch. Network I/O goes through the **netpoller** (epoll on Linux, kqueue on macOS).
- A goroutine in a blocking system call keeps its M, but the runtime hands that M's P to another M (`sysmon` retakes it), so other goroutines keep running.
- **Preemption:** since Go 1.14, goroutines are preempted asynchronously. `sysmon` sends the thread a signal (`SIGURG`) when a goroutine has run for ~10 ms, so even a tight loop can't hold a P forever.
- `runtime.LockOSThread()` wires a goroutine to its M, for code that needs thread identity (thread-local C state, GUI main threads on macOS, Linux namespace calls).

### What a goroutine has, and doesn't

| Has | Doesn't have |
|---|---|
| its own stack (2 KB at start, growable) | an ID visible to the program: there's deliberately no `GoroutineID()` (IDs appear only in tracebacks) |
| access to all the process's memory | ownership of anything: no handles, no credentials, no resources of its own |
| whatever its closure captured, plus arguments | a name, priority or CPU affinity |
| **pprof labels**, which a new goroutine copies from its creator | a way to be killed from outside |

### Goroutines have no parent

- **No parent/child relationship.** `go f()` creates a goroutine that's independent of its creator: when the creator returns, panics or blocks, the new one carries on. The only record of the creator is in tracebacks ("created by pkg.fn in goroutine 7").
- **They all belong to the process.** When `main` returns, or `os.Exit` is called, the program exits and every goroutine stops immediately, mid-statement, with no deferred calls run.
- **No external kill.** One goroutine can't stop another. Stopping is **cooperative**: the goroutine has to watch something, usually a `context.Context` (`<-ctx.Done()`), a closed channel, or a flag. Structure (who waits for whom) is built by the program with `sync.WaitGroup`, `errgroup`, channels, and context trees, where cancelling a parent context cancels every context derived from it.
- **A panic is process-wide.** An unrecovered panic in *any* goroutine crashes the whole program. `recover` only works in a deferred function of the goroutine that panicked, so a goroutine can't catch another's panic.
- **Leaks are silent.** A goroutine blocked forever (on a channel nobody sends to) stays alive, holding its stack and whatever it references. The garbage collector doesn't collect goroutines.

### Communication

Goroutines share memory, like threads. Go's idiom is "share memory by communicating": pass ownership of data through **channels**, so only one goroutine touches it at a time. `sync.Mutex`, `sync/atomic` and friends are there for shared state. The race detector (`go test -race`) finds unsynchronized access.

### Cost

Creating a goroutine costs about a microsecond and a few KB, and switching between goroutines happens in user space (tens to hundreds of nanoseconds). A program can run hundreds of thousands to millions of them.

## 5. Side by side

| | Process | Thread | Goroutine |
|---|---|---|---|
| Managed by | the kernel | the kernel | the Go runtime (on kernel threads) |
| Isolation | own address space (MMU) | shares its process's | shares its process's |
| Identity | PID | TID | none exposed to the program |
| Owns | memory, fds, credentials, limits | its stack and registers | its stack |
| Created by | `fork`/`exec`, `posix_spawn`, `clone` | `pthread_create` (`clone` on Linux) | `go f()` |
| Parent | yes: exit status, `SIGCHLD`, inheritance; orphans go to PID 1 | none (all threads are peers in their process) | none; all belong to the process |
| Stopped by | signals, preemptively; the kernel reclaims its resources | exits itself, or dies with its process | returns itself (cooperative cancellation), or dies with the process |
| A crash | kills the process | kills the process | an unrecovered panic kills the process |
| Scheduling | via its threads | kernel scheduler (EEVDF on Linux; QoS-driven on macOS) | Go scheduler: M:N, work stealing, async preemption |
| Switch cost | kernel switch + address space change | kernel switch | user-space switch |
| Typical count | hundreds to thousands | thousands | up to millions |
| Communication | pipes, sockets, shared memory, signals, Mach ports | shared memory + locks | channels, or shared memory + `sync` |
