# godeep's TamaGo runtime patch

`godeep-runtime.patch` applies to [tamago-go](https://github.com/usbarmory/tamago-go)
at the commit `~/tamago-go` is checked out at (tamago-go1.27.4). `make tamago-go`
builds the patched toolchain in `bin/tamago-go`, which the Makefile then uses
instead of `~/tamago-go`.

## What it changes (src/runtime)

With several cores, TamaGo's Ms never park: an idle M sleeps in `goos.Idle`
still holding its P. Go's wake-up paths assume new work goes to an idle P or
a spinning M, so an idle core used to notice new work, a stop-the-world or a
GC worker request only when its own idle sleep ended.

- `wakep`, finding no idle P, wakes one idle core (`tamagoKick`, `goos.Wake`).
- `preemptall` wakes every idle core (`tamagoKickAll`); `preemptone` wakes
  an idle target M (`tamagoWakeM`).
- `beforeIdle` records the core in an idle mask (set before its last check
  for work, so a waker can't miss it) and sleeps at most 10 ms while another
  core is running.

(Making an M stop spinning before it sleeps was tried and dropped: it hung a
single-core boot, `make check-linklocal`, for reasons not found.)

Measured on QEMU virt, HVF, 4 cores, with board/virt's boot-core SGI fix
(2026-11-27):

| | stock | patched |
|---|---|---|
| `yes \| head -n 100000 \| wc -l` | 1.0-6.0 s | 38 ms (1 core: 42 ms) |
| `yes \| head -n 1000000 \| wc -l` | 11.2 s | 0.35-0.37 s |
| `yes \| grep y \| head -n 200000 \| wc -l` | 3.6 s | 86 ms |
| `burn 4 3` | 8.2 GB/s | 8.6 GB/s |

Not done: preempting a goroutine that never yields (that needs a timer
interrupt on every core; secondaries run with IRQs masked). Some wake-ups
are still lost under load; board/virt's 100 ms `maxIdle` bounds them.

## CPU faults as panics (sigpanic_tamago*.go/.s)

On TamaGo a CPU fault (a nil pointer read: a data abort) went to TamaGo's
exception handler, which panicked from a frame the unwinder doesn't know
and left IRQs masked; the second fault in a boot halted the machine. The
patch adds what the signal-based ports have:

- `runtime.sigpanic` for tamago/arm64: `invalid memory address or nil
  pointer dereference` (a `runtime.Error`) for aborts, a panic for alignment
  faults and undefined instructions, and a throw when the fault hit the
  runtime itself (`canpanic`).
- `runtime·tamagoFault` (pushed with `//go:linkname` for TamaGo's vectors):
  records ESR/FAR/ELR in `g`, restores the interrupted code's DAIF, arranges
  the stack as `sigctxt.preparePanic` does, and branches to sigpanic, so the
  faulting frame is unwound as trapped (`FuncID_sigpanic`).

third_party/tamago's vectors use it with the `godeepruntime` build tag
(set by the Makefile when the toolchain has these files) and the board's
`arm64.FaultPanics`. Tested: `make check-crash` (seven kinds of fault in a
row, a self-recovered fault, 8000 faults recovered with GC running on 1
and 4 cores). See docs/UnderstandingGoRoutineFailure.md.
