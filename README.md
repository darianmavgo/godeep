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
- **Bare metal:** on QEMU's `microvm` (x86-64) virtual machine, via [TamaGo](https://github.com/usbarmory/tamago), which runs the Go runtime with no OS underneath.
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

You need three things: Go, the TamaGo version of Go, and QEMU.

```sh
brew install go qemu
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
booting godeep... (quit QEMU: Ctrl-A then X)
SeaBIOS (version ...)
Booting from ROM..godeep 0.0.2 (tamago/amd64, go1.27.4-X:simd, GOMAXPROCS=1)
type 'help' for commands
godeep$
```

This is a real x86-64 kernel image (`bin/godeep.elf`). QEMU boots it with no Linux and no macOS underneath; Go's runtime *is* the kernel. The boot is a bit slow on Apple Silicon because QEMU emulates an Intel CPU.

To get out of the VM, either:
- type `halt`, or
- press **Ctrl-A**, then **X**.

> Everything below works the same in both modes. One difference to notice: hosted shows `GOMAXPROCS=10` (your Mac's cores), and the VM shows `GOMAXPROCS=1` (one virtual CPU).

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
heap in use        640.0 KiB
heap objects       1665
total from system  4.9 MiB
GC cycles          0
goroutines         10
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
6    0    uart0         sleep         -        microvm.NewConsole.func1
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
- Don't busy-loop without blocking. The VM has one CPU, and a goroutine that never blocks or calls into the runtime can starve the others.

---

## 8. How it works inside

### Boot sequence (bare metal)

```
QEMU → SeaBIOS → loads bin/godeep.elf
  → TamaGo: CPU setup, timers, memory, starts the Go runtime
  → board/microvm: initializes the serial port (COM1)
  → cmd/godeep/main.go: main()
      → microvm.NewConsole()   starts kernel goroutine "uart0" (driver)
      → kernel.Boot()
          → tty.New()          starts kernel goroutine "tty" (line discipline)
          → spawn PID 1 init
              → spawn PID 2 sh
```

### A keystroke's journey

```
you press 'p'
  → QEMU puts the byte on the virtual COM1 serial port
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
| `board/microvm/console.go` | serial driver goroutine |
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
board/microvm/       QEMU microvm drivers   (//go:build tamago)
board/hosted/        host-OS shims          (//go:build !tamago)
docs/architecture.md design and roadmap
CLAUDE.md            rules and notes for AI-assisted development
```

### Make targets

| Command | What it does |
|---|---|
| `make test` | `go vet` + `go test ./...` on your Mac |
| `make hosted` | build and run godeep as a Mac process |
| `make kernel` | build `bin/godeep.elf` with TamaGo |
| `make qemu` | build, then boot in QEMU |
| `make clean` | delete `bin/` |

Development loop: write code, run `make test`, try it with `make hosted`, then confirm on bare metal with `make qemu`.

---

## 10. Troubleshooting

| Symptom | Fix |
|---|---|
| `make kernel`: `go: no such file` / wrong toolchain | TamaGo isn't at `~/tamago-go/bin/go`. Build it (step 1) or pass `TAMAGO=...`. |
| QEMU stops after `Booting from ROM..` with no output | The VM needs **at least 3 GB RAM** (`-m 3G`, already in the Makefile). With less, the kernel's stack lands in missing memory. |
| ^C quit QEMU instead of stopping my program | You ran QEMU by hand without `signal=off`. Use `make qemu`. |
| Can't exit QEMU | **Ctrl-A**, then **X**, or type `halt`. |
| A process won't die after `kill` | One of its goroutines ignores `ctx` (see section 6). `ps -g PID` shows where it's stuck. |
| Debugging a crash during boot | Add `-d int,cpu_reset` to `QEMU_FLAGS` and look for `Triple fault` and the instruction pointer just before it. |

---

## 11. Roadmap

See `docs/architecture.md` for the full plan:
- **Phase 2:** capabilities and IPC. Processes get only the handles they're given, plus named services over channels.
- **Phase 3:** a filesystem (`/proc`, `/dev`, in-memory files) and shell pipes (`ls | cat`) built on channels.
- **Phase 4:** virtio disk and networking (gVisor's pure-Go TCP/IP stack), and interrupt-driven drivers.
- **Phase 5:** load programs at runtime as WebAssembly (wazero), each one a goroutine-backed process.
- **Phase 6:** per-process resource limits and hardening.
