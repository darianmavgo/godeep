package coreutils

import (
	"context"
	"fmt"
	"runtime"

	"godeep/kernel/proc"
	"godeep/kernel/prog"
)

func init() {
	prog.Register(prog.Program{Name: "crash", Usage: "crash panic|nil|write|leaf|frame|recover|storm|child: fail on purpose, to check that only this process dies (docs/UnderstandingGoRoutineFailure.md)", Main: crash})
}

// crashTarget is nil: reading through it makes the CPU take a data abort on
// the bare-metal kernel (a Go nil check is a load that faults: page 0 is
// unmapped, pgtable.NilGuard), a signal on a hosted one.
var crashTarget *[1 << 10]uint64

// crashLeaf faults in a leaf function without a frame: the unwinder must
// find its caller from the LR the fault entry saved.
//
//go:noinline
func crashLeaf(p *[1 << 10]uint64) uint64 { return p[7] }

// crashFrame faults in a leaf function with a frame.
//
//go:noinline
func crashFrame(p *[1 << 10]uint64, n int) uint64 {
	var a [16]uint64
	for i := range a {
		a[i] = uint64(i * n)
	}
	return a[n&15] + p[n]
}

// crash fails the way its argument says:
//
//	panic    a Go panic in the main goroutine
//	nil      a nil pointer read (a CPU fault, data abort, on bare metal)
//	write    a nil pointer write
//	leaf     a nil read in a frameless leaf function
//	frame    a nil read in a leaf function with a frame
//	recover  a nil read that the program recovers from itself (exits 0)
//	storm    8 goroutines x 500 faults recovered, with GC running
//	child    a nil read in a goroutine the process started (p.Go)
func crash(ctx context.Context, p *proc.Proc) error {
	what := "panic"
	if len(p.Args) > 0 {
		what = p.Args[0]
	}
	switch what {
	case "panic":
		panic("crash: on purpose")
	case "nil":
		fmt.Fprintln(p.Stdout, "crash: reading through a nil pointer")
		return fmt.Errorf("crash: read %d: no fault", crashTarget[7])
	case "write":
		fmt.Fprintln(p.Stdout, "crash: writing through a nil pointer")
		crashTarget[9] = 1
		return fmt.Errorf("crash: write: no fault")
	case "leaf":
		return fmt.Errorf("crash: leaf read %d: no fault", crashLeaf(crashTarget))
	case "frame":
		return fmt.Errorf("crash: frame read %d: no fault", crashFrame(crashTarget, len(p.Args)))
	case "recover":
		v, err := func() (v uint64, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			return crashLeaf(crashTarget), nil
		}()
		if err == nil {
			return fmt.Errorf("crash: read %d: no fault", v)
		}
		fmt.Fprintf(p.Stdout, "crash: recovered: %v\n", err)
		return nil
	case "storm":
		// many goroutines fault and recover while the GC runs: the stack
		// scans during each panic must handle the faulting frames
		const goroutines, rounds = 8, 500
		errs := make(chan error, goroutines)
		for g := 0; g < goroutines; g++ {
			p.Go(func(context.Context) {
				keep := make([][]byte, 0, 16)
				for i := 0; i < rounds; i++ {
					keep = append(keep, make([]byte, 4096))
					if len(keep) == cap(keep) {
						keep = keep[:0]
					}
					func() {
						defer func() { recover() }()
						if i%2 == 0 {
							crashLeaf(crashTarget)
						} else {
							crashFrame(crashTarget, i)
						}
					}()
					if i%50 == 0 {
						runtime.GC()
					}
					runtime.Gosched()
				}
				errs <- nil
			})
		}
		for g := 0; g < goroutines; g++ {
			if err := <-errs; err != nil {
				return err
			}
		}
		fmt.Fprintf(p.Stdout, "crash: storm: %d faults recovered\n", goroutines*rounds)
		return nil
	case "child":
		fmt.Fprintln(p.Stdout, "crash: reading through a nil pointer in a child goroutine")
		p.Go(func(context.Context) { _ = crashLeaf(crashTarget) })
		<-ctx.Done()
		return context.Cause(ctx)
	}
	return fmt.Errorf("crash: unknown %q (panic, nil, write, leaf, frame, recover, storm, child)", what)
}
