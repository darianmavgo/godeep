# godeep

A kernel and operating system written in Go, where **Go's concurrency model is the process model**.

In a normal OS, a process is an address space plus threads. The kernel switches between processes using hardware: page tables, interrupts and context switches. godeep throws that out:

| Classic OS | godeep |
|---|---|
| process | a group of goroutines sharing a PID and a `context.Context` |
| scheduler | the Go runtime scheduler |
| `kill` | cancel the process's context |
| IPC / pipes | Go channels |
| memory protection (MMU) | Go's memory safety |
| system call | an ordinary Go function call |

It runs in two ways:
- **Bare metal:** on a virtual machine via [TamaGo](https://github.com/usbarmory/tamago), which runs the Go runtime with no OS underneath. The default target is **arm64 on QEMU's `virt` machine, accelerated by Apple's Hypervisor.framework (HVF)**, so it runs at native speed on Apple Silicon. An x86-64 target (QEMU `microvm`, emulated) is also available.
- **Hosted:** as a normal program on your Mac or Linux machine, for fast development.

This README is a tutorial. Work through it top to bottom.

---

## Contents

1. [Set up your machine](#1-set-up-your-machine)
2. [First boot](#2-first-boot)
3. [Tour the shell](#3-tour-the-shell)
4. [See processes as goroutines with `ps`](#4-see-processes-as-goroutines-with-ps)
5. [Background jobs, ^C and kill](#5-background-jobs-c-and-kill)
6. [The catch: kill is cooperative](#6-the-catch-kill-is-cooperative)
7. [Write your own program](#7-write-your-own-program)
8. [How it works inside](#8-how-it-works-inside)
9. [Project layout](#9-project-layout)
10. [Troubleshooting](#10-troubleshooting)
11. [Roadmap](#11-roadmap)

---

## 1. Set up your machine

You need an **Apple Silicon Mac** for the default setup: the VM uses Apple's Hypervisor.framework (HVF) to run arm64 code natively. On any other machine everything still works, just emulated (see `ACCEL=tcg` below).

Install three things: Go, the TamaGo version of Go, and QEMU.

```sh
brew install go qemu   # qemu includes qemu-system-aarch64 (arm64) and qemu-system-x86_64
```

TamaGo is a fork of the Go toolchain that adds `GOOS=tamago` (Go with no OS). There's no macOS download, so build it from source; it takes about a minute:

```sh
git clone --depth 1 --branch tamago-go1.27.4 https://github.com/usbarmory/tamago-go ~/tamago-go
cd ~/tamago-go/src && ./make.bash
```

The Makefile expects it at `~/tamago-go/bin/go`. If you put it elsewhere, pass `TAMAGO=/path/to/bin/go` to `make`.

> Keep the tamago-go version matching the `github.com/usbarmory/tamago` version in `go.mod`.

Check the setup by running the tests:

```sh
cd ~/Documents/godeep
make test
```

Every package should print `ok` or `[no test files]`.

---

## 2. First boot

### Option A: hosted (instant, good for learning)

```sh
make hosted
```

```
godeep 0.0.2 (darwin/arm64, go1.27.4, GOMAXPROCS=10)
type 'help' for commands
godeep$
```

That's the whole OS running as a Mac process. **Ctrl-D** shuts it down.

### Option B: bare metal in a VM

```sh
make qemu
```

```
booting godeep arm64... (quit QEMU: Ctrl-A then X, or type halt)
godeep 0.0.2 (tamago/arm64, go1.27.4-X:simd, GOMAXPROCS=1)
type 'help' for commands
godeep$
```

This is a real arm64 kernel image (`bin/godeep-arm64.elf`). QEMU boots it on its `virt` machine using your Mac's hardware virtualization (`-accel hvf -cpu host`), with no Linux and no macOS underneath; Go's runtime *is* the kernel. It boots in about a second and uses almost no CPU while idle.

Other ways to run it:

| Command | What it runs |
|---|---|
| `make qemu ARCH=amd64` | the x86-64 kernel on QEMU `microvm`. Slower to boot: QEMU emulates an Intel CPU on Apple Silicon |
| `make qemu ACCEL=tcg` | the arm64 kernel without hardware acceleration (for Linux or Intel hosts) |

To get out of the VM:

| From | Do | What happens |
|---|---|---|
| inside godeep | type `halt` | powers off immediately |
| the QEMU window | press **Ctrl-A**, then **X** | QEMU quits immediately |
| **another terminal** | `make stop` | presses the VM's **power button**: godeep kills every process, reports any that won't exit, and powers off. If the guest doesn't respond within 5 s, it falls back to killing QEMU |
| another terminal | `make kill` | kills QEMU immediately (SIGKILL) |

`make stop` finds the VM through `bin/qemu-arm64.pid` and the control socket `bin/qmp-arm64.sock`, which `make qemu` creates. It also finds VMs started some other way, by matching QEMU's command line. The hosted build shuts down the same clean way on `SIGTERM` (e.g. `kill <pid>`).

> Everything below works the same in every mode. One difference to notice: hosted shows `GOMAXPROCS=10` (your Mac's cores), and the VM shows `GOMAXPROCS=1` (one virtual CPU; TamaGo doesn't start extra arm64 cores yet).

---

## 3. Tour the shell

Type `help`:

```
godeep$ help
BUILTIN   DESCRIPTION
help      list commands
ps        ps [-g] [PID]: processes, or goroutines with -g
kill      kill PID...: cancel processes
uptime    time since boot
mem       memory and scheduler stats
exit      exit this shell (init starts a new one)
halt      power off
date      wall-clock time
lsdev     devices, their drivers and state
irqstat   interrupt lines and counts
irqlat    irqlat [N]: measure interrupt-to-goroutine latency
dmastat   DMA memory usage

PROGRAM   USAGE
countdown countdown [N]: count down from N using two goroutines
echo      echo ARGS...
leak      leak: start a goroutine that ignores kill (cooperative-kill demo)
pingpong  pingpong [N]: two goroutines pass a ball over a channel
sleep     sleep SECONDS
workers   workers [N]: N goroutines doing periodic work until killed
```

- **Builtins** run inside the shell.
- **Programs** run as new processes.

Try a few:

```
godeep$ echo hello kernel
hello kernel
godeep$ pingpong 3
ping 0
  pong 0
ping 1
  pong 1
ping 2
  pong 2
godeep$ uptime
up 41.3s
godeep$ mem
heap in use        552.0 KiB
heap objects       506
total from system  4.9 MiB
GC cycles          0
goroutines         9
GOMAXPROCS         1
```

`pingpong` is two goroutines in one process, handing a counter back and forth over channels. That's IPC in godeep: no pipes or sockets, just channels.

Now try `exit`:

```
godeep$ exit
init: sh (pid 2) exited; respawning
type 'help' for commands
godeep$
```

PID 1 (`init`) supervises the shell. When the shell dies, init starts a new one, just like `getty` respawning a login on Unix.

---

## 4. See processes as goroutines with `ps`

This is the heart of godeep. Start some background work, then look:

```
godeep$ workers 3 &
[3] workers
godeep$ ps
PID  PPID  NAME          STATE    UPTIME  GOROUTINES  GOROUTINE STATES
-    -     [go runtime]  -        15.7s   1           1 select
0    -     [kernel]      running  15.7s   2           1 chan receive, 1 sleep
1    0     init          running  15.7s   1           1 select
2    1     sh            running  15.7s   1           1 running
3    2     workers       running  2s      4           4 select
```

How to read it:
- **workers (PID 3)** is one process made of **4 goroutines**: a main goroutine plus 3 workers. All 4 are blocked in `select`, waiting on a timer or channel. They use no CPU while blocked; the Go scheduler simply doesn't run them.
- **sh (PID 2)** is `running`, because it's the goroutine executing `ps` right now.
- **[kernel] (PID 0)** holds the kernel's own goroutines, which are device drivers.
- **[go runtime]** counts goroutines that belong to no process, such as Go's own housekeeping.

Now zoom into individual goroutines with `-g`:

```
godeep$ ps -g
GID  PID  PROC          STATE         WAITING  FUNCTION
1    -    [go runtime]  select        -        proc.(*Proc).Wait
6    0    uart0         sleep         -        virt.NewConsole.func1
7    0    tty           chan receive  -        tty.(*TTY).discipline
8    1    init          select        -        proc.(*Proc).Wait
9    2    sh            running       -        proc.Snapshot
10   3    workers       select        -        coreutils.workers
11   3    workers       select        -        coreutils.workers.func1
12   3    workers       select        -        coreutils.workers.func1
13   3    workers       select        -        coreutils.workers.func1
```

| Column | Meaning |
|---|---|
| `GID` | the goroutine's ID in the Go runtime |
| `PID` / `PROC` | which godeep process owns it |
| `STATE` | what the goroutine is doing: `running`, `runnable` (waiting for a CPU), `select`, `chan receive`, `sleep`, … |
| `WAITING` | how long it has been blocked (Go only reports this after about a minute) |
| `FUNCTION` | where it is in your code (runtime-internal frames are skipped) |

Look at PID 0:
- **`uart0`** is the serial-port driver. It's one goroutine that owns the hardware and turns incoming keystrokes into channel sends.
- **`tty`** reads that channel and assembles lines for the shell.

Drivers in godeep are just goroutines.

Device drivers go one step further: each one is a **process**. `devmgr` starts one `drv/<name>` per device once its dependencies are ready, and restarts it if it crashes. Try:

```
godeep$ lsdev
DEVICE  COMPATIBLE              STATE  PID  RESTARTS  IRQ  REG        INFO
gpio0   arm,pl061               ready  4    0         39   0x9030000
ptimer  arm,armv8-ptimer-probe  ready  7    0         30   -
rtc     arm,pl031               ready  5    0         34   0x9010000  2026-11-25T21:12:41Z
virtio  virtio,mmio-bus         ready  -    0         -    0xa000000  no devices attached
godeep$ irqlat 200
timer deadline -> driver goroutine, 200 samples:
  min 96.291µs  median 319.708µs  avg 333.972µs  p99 933.791µs  max 1.233208ms
```

`irqstat` shows interrupt counts, and `dmastat` shows the DMA memory region. The design is in `docs/DriverModel.md`.

`ps -g 3` shows only PID 3's goroutines.

> **Where does this data come from?** No special library. Go's `runtime.Stack(buf, true)` dumps every goroutine, and since Go 1.27 the dump includes each goroutine's *pprof labels*. godeep labels every process's first goroutine with `pid` and `proc`. Go copies labels to child goroutines automatically, so every goroutine a process starts is attributed to it. `kernel/proc/goroutines.go` parses the dump.

---

## 5. Background jobs, ^C and kill

There are three ways to stop a process.

**^C on a foreground program:**

```
godeep$ sleep 30
^C
godeep$
```

**`kill` on a background one:**

```
godeep$ workers 3 &
[3] workers
godeep$ kill 3
godeep$ ps
```

`workers` is gone, along with all 4 of its goroutines.

**The program finishes:** when a program's main function returns, its process ends, like `main()` in Go.

What `kill` actually does is **cancel the process's `context.Context`**:
- Every goroutine in the process is watching `ctx.Done()`, so each one sees the cancellation and returns.
- Once the last one has returned, the process is removed from the table.

There are no signals and no hardware interrupt, just Go's normal cancellation pattern.

You can keep typing while a foreground program runs. Your input is queued, because the `tty` goroutine collects it separately from whoever is reading.

---

## 6. The catch: kill is cooperative

Go has **no way to forcibly stop a goroutine from outside**. godeep can only *ask*, by cancelling the context. A goroutine that never checks its context can't be killed.

The `leak` program demonstrates this on purpose:

```
godeep$ leak
pid 6 exits now; its goroutine keeps running (see ps)
godeep$ ps
PID  PPID  NAME      STATE     UPTIME  GOROUTINES  GOROUTINE STATES
...
6    ?     leak      orphaned  -       1           1 sleep
```

The `leak` process has exited, but a goroutine it started with a bare `go` and that ignores its context is still alive. It still carries the `pid=6` label, so `ps` can show you the orphan.

This is why godeep has two rules:
1. Every blocking operation must also watch `ctx.Done()`.
2. Start goroutines with `p.Go(...)`, never a bare `go`.

The tradeoff: in exchange for this rule, there's no context-switch cost and no kernel/user boundary.

---

## 7. Write your own program

Programs are compiled into the kernel image. Let's read the one that ships as an example: `programs/hello/hello.go`.

```go
package hello

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"godeep/kernel/proc"
	"godeep/kernel/prog"
)

// 1. Register the program at startup. The shell finds it by name.
func init() {
	prog.Register(prog.Program{
		Name:  "countdown",
		Usage: "countdown [N]: count down from N using two goroutines",
		Main:  countdown,
	})
}

// 2. The entry point. ctx is the process's lifetime: when it's cancelled
//    (kill, ^C), return. p is your process: PID, Args, Stdout, Go().
func countdown(ctx context.Context, p *proc.Proc) error {
	n := 5
	if len(p.Args) > 0 {
		if v, err := strconv.Atoi(p.Args[0]); err == nil {
			n = v
		}
	}

	ticks := make(chan int)

	// 3. Start a second goroutine *in this process* with p.Go.
	p.Go(func(ctx context.Context) {
		defer close(ticks)
		for i := n; i > 0; i-- {
			select {
			case ticks <- i:
			case <-ctx.Done(): // 4. killed? stop promptly.
				return
			}
			time.Sleep(time.Second)
		}
	})

	// 5. Main goroutine: receive over the channel and print.
	for i := range ticks {
		fmt.Fprintf(p.Stdout, "%d...\n", i)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fmt.Fprintln(p.Stdout, "liftoff!")
	return nil
}
```

The program is linked in with one line in `kernel/kernel.go`:

```go
_ "godeep/programs/hello"
```

Run it:

```
godeep$ countdown 3
3...
2...
1...
liftoff!
godeep$ countdown 30 &
[4] countdown
godeep$ ps -g 4
GID  PID  PROC       STATE         WAITING  FUNCTION
6    4    countdown  chan receive  -        hello.countdown
33   4    countdown  sleep         -        hello.countdown.func1
godeep$ kill 4
```

You can see both of your goroutines: `main` blocked receiving from the channel, and the producer sleeping.

### Your turn

Make a new program in four steps:

1. Create `programs/<yourname>/<yourname>.go` with an `init()` that calls `prog.Register`.
2. Add `_ "godeep/programs/<yourname>"` to the imports in `kernel/kernel.go`.
3. Run `make hosted` and try it.
4. Run `make qemu` to try it on bare metal.

Ideas to practice with:
- **`primes N`:** a pipeline of goroutines, one filter per prime (the classic Go prime sieve). Watch `ps -g` while it runs.
- **`stress N`:** start N goroutines, and see what `mem` and `ps` show.
- **`crash`:** call `panic("oops")` inside a `p.Go` goroutine. Only your process dies; the shell and kernel survive. That's panic containment.

Rules for program code:
- Take `ctx` seriously: every blocking `select` needs a `case <-ctx.Done()`.
- Use `p.Go`, not `go`.
- Write output to `p.Stdout`.
- Don't busy-loop without blocking. The VM has one CPU, so a goroutine that never blocks or calls into the runtime can starve the others, and it keeps your Mac's core at 100%.

---

## 8. How it works inside

### Boot sequence (bare metal, arm64)

```
QEMU virt (HVF) → loads bin/godeep-arm64.elf, jumps to it at EL1
  → TamaGo arm64: MMU on, exception vectors, starts the Go runtime
  → board/virt Init (Hwinit1): caches, generic timer, PL011 UART,
                               GIC + timer-based idle, PSCI power-off
  → cmd/godeep/main.go: main()
      → virt.NewConsole()      starts kernel goroutine "uart0" (driver)
      → kernel.Boot()
          → tty.New()          starts kernel goroutine "tty" (line discipline)
          → spawn PID 1 init
              → spawn PID 2 sh
```

### The arm64 board (`board/virt`)

TamaGo ships arm64 CPU support (boot, MMU, exceptions, timers, interrupt controller) but **no board for QEMU's `virt` machine**, so godeep has its own. A TamaGo board is a small package that fills in hooks the Go runtime links against with `//go:linkname`:

| Runtime hook | What `board/virt` provides |
|---|---|
| `RamStart`, `RamSize` | RAM at `0x40000000`, 4 GB (must not exceed QEMU's `-m`) |
| `Hwinit1` | enables caches, the generic timer, the PL011 UART, the GIC and the idle hook |
| `Nanotime` | reads the arm64 generic timer |
| `Printk` | sends runtime output (`println`, panics) to the UART |
| `InitRNG`, `GetRandomData` | an AES-CTR random generator (see the caveat below) |
| `Exit` | PSCI `SYSTEM_OFF`, which is why `halt` closes QEMU |

Two details are worth knowing:

- **Idle sleeps instead of spinning.** When every goroutine is blocked, the Go runtime calls the board's idle hook with the next timer deadline. godeep arms the CPU's virtual timer for that deadline and executes `WFI` (wait for interrupt). Under HVF, QEMU puts the virtual CPU to sleep until the timer fires. An idle godeep VM uses about 1.5% of one Mac core; without this it used 100%.
- **The random seed isn't secure.** It comes from the timer counter at boot. That's fine for development, but don't rely on `crypto/rand` until a hardware RNG (virtio-rng) is added.

The x86-64 target (`ARCH=amd64`) uses TamaGo's ready-made `microvm` board instead, through `board/microvm`.

### A keystroke's journey

```
you press 'p'
  → QEMU puts the byte in the virtual PL011 serial port (COM1 on amd64)
  → uart0 goroutine (polls every 5 ms) reads it → sends on a channel
  → tty goroutine receives it, echoes 'p', adds it to the current line
  → Enter: tty sends the finished line on another channel
  → sh goroutine receives "ps", runs it
```

Every arrow after the hardware is a Go channel. That's the "concurrency layer": the parts of the OS talk by passing messages between goroutines that each own one thing.

### Key code

| File | What it does |
|---|---|
| `kernel/proc/proc.go` | `Proc`, `Table`, `Spawn`, `Kill`, `p.Go`, panic containment, process reaping |
| `kernel/proc/goroutines.go` | `Snapshot()`: parses the runtime's goroutine dump for `ps` |
| `kernel/tty/tty.go` | line discipline: echo, backspace, ^C, typeahead |
| `programs/sh/` | the shell and its builtins (`ps.go` is the `ps` command) |
| `programs/initd/` | PID 1 |
| `board/virt/` | arm64 board: RAM layout, PL011 serial driver, timer, idle, RNG, power-off |
| `board/microvm/console.go` | amd64 serial driver goroutine |
| `board/hosted/console.go` | the same interface on top of macOS stdin/stdout |

---

## 9. Project layout

```
cmd/godeep/          bare-metal entry point (built with GOOS=tamago)
cmd/godeep-hosted/   host entry point (normal Go)
kernel/              portable core: never imports board/*
  proc/              processes, goroutine inspection
  tty/               console line discipline
  prog/              registry of built-in programs
programs/            init, sh, coreutils, hello
board/virt/          QEMU virt arm64 board  (//go:build tamago && arm64), the default
board/microvm/       QEMU microvm x86-64    (//go:build tamago && amd64)
board/hosted/        host-OS shims          (//go:build !tamago)
docs/architecture.md design and roadmap
CLAUDE.md            rules and notes for AI-assisted development
```

### Make targets

| Command | What it does |
|---|---|
| `make test` | `go vet` + `go test ./...` on your Mac |
| `make hosted` | build and run godeep as a Mac process |
| `make kernel` | build `bin/godeep-arm64.elf` with TamaGo (`ARCH=amd64` for x86-64) |
| `make image` | turn it into an arm64 Linux-style Image (`bin/godeep-arm64.img`), the format m1n1 and U-Boot load; `make qemu` boots this by default, with the device tree in `x0` |
| `make qemu EL2=1` | boot entered at EL2, the way m1n1 starts kernels on a real Mac (emulated, TCG) |
| `make image BOARD=applem1` | the Image for this MacBook Pro (`bin/godeep-applem1.img`); boot it from the godeep partition (docs/TargetMyMac.md §6.8). Not yet tested on hardware |
| `make qemu` | build, then boot in QEMU (arm64 + HVF by default) |
| `make stop` | from another terminal: power godeep off cleanly, falling back to killing QEMU |
| `make kill` | from another terminal: kill QEMU immediately |
| `make clean` | delete `bin/` |

Development loop: write code, run `make test`, try it with `make hosted`, then confirm on bare metal with `make qemu`.

---

## 10. Troubleshooting

| Symptom | Fix |
|---|---|
| `make kernel`: `go: no such file` / wrong toolchain | TamaGo isn't at `~/tamago-go/bin/go`. Build it (step 1) or pass `TAMAGO=...`. |
| `-accel hvf` error (e.g. `HV_UNSUPPORTED`, `HV_DENIED`) | HVF needs an Apple Silicon Mac and a QEMU with the hypervisor entitlement (Homebrew's has it). Elsewhere, use `make qemu ACCEL=tcg`. |
| `make qemu ARCH=amd64` is slow to boot | Expected: your Mac emulates the x86 CPU. Use the default arm64 target for daily work. |
| amd64: QEMU stops after `Booting from ROM..` with no output | The microvm VM needs **at least 3 GB RAM** (`-m 3G`, already in the Makefile). With less, the kernel's stack lands in missing memory. |
| The VM uses a full CPU core while idle | Under HVF this is a QEMU 11.1.x bug that fires after a device interrupt; godeep avoids it by polling the UART there (see `docs/DriverModel.md`). Otherwise, check that `board/virt` sets `goos.Idle = idle` and that QEMU uses `gic-version=3`. |
| ^C quit QEMU instead of stopping my program | You ran QEMU by hand without `signal=off`. Use `make qemu`. |
| Can't exit QEMU | **Ctrl-A**, then **X**, or type `halt`. From another terminal: `make stop` (or `make kill`). |
| `make stop` says "no response from the guest" | Expected on `ARCH=amd64`: godeep doesn't handle the x86 (ACPI) power button yet, so it falls back to SIGTERM. On arm64 it means godeep is hung; the fallback still stops QEMU. |
| A process won't die after `kill` | One of its goroutines ignores `ctx` (see section 6). `ps -g PID` shows where it's stuck. |
| Debugging a crash during boot | Use `ACCEL=tcg` and add `-d int,cpu_reset,guest_errors` to `QEMU_FLAGS`. That logs exceptions with the PC (arm64) or a `Triple fault` with the RIP (amd64). |

---

## 11. Roadmap

See `docs/architecture.md` for the full plan:
- **Phase 2:** capabilities and IPC. Processes get only the handles they're given, plus named services over channels.
- **Phase 3:** a filesystem (`/proc`, `/dev`, in-memory files) and shell pipes (`ls | cat`) built on channels.
- **Phase 4:** virtio disk and networking (gVisor's pure-Go TCP/IP stack), and interrupt-driven drivers.
- **Phase 5:** load programs at runtime as WebAssembly (wazero), each one a goroutine-backed process.
- **Phase 6:** per-process resource limits and hardening.

arm64 follow-ups:
- **Multi-core:** start the extra CPU cores so `GOMAXPROCS` > 1 (TamaGo has no arm64 SMP bring-up yet, so the VM runs with `-smp 1`).
- **virtio-rng** for real entropy.
- **Interrupt-driven UART** through the GIC, replacing the 5 ms keyboard poll.

Design notes:
- [`ReUsingGVisor.md`](ReUsingGVisor.md): networking, filesystems and which "syscalls" are worth building
- [`GoGpuSupport.md`](GoGpuSupport.md): options for display and GPU compute
- [`docs/DriverModel.md`](docs/DriverModel.md): driver rules, DMA, interrupt bridge, device manager, RTKit
- [`docs/TargetMyMac.md`](docs/TargetMyMac.md): this Mac's hardware, Go driver coverage, the driver-foundation status, and how to carve out a godeep boot entry + data partition next to macOS and Omarchy (§6)
