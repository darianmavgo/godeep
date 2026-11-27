package coreutils

import (
	"context"
	"crypto/sha256"
	"fmt"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"godeep/kernel/proc"
	"godeep/kernel/prog"
)

func init() {
	prog.Register(prog.Program{Name: "burn", Usage: "burn [GOROUTINES] [SECONDS]: hash on N goroutines (4) for S seconds (3), print MB/s: a CPU-bound load, and an SMP speed-up check", Main: burn})
}

// burn keeps N goroutines busy with SHA-256 and reports the total rate.
// With GOMAXPROCS cores, N >= cores should scale close to linearly.
func burn(ctx context.Context, p *proc.Proc) error {
	n, secs := 4, 3.0
	if len(p.Args) > 0 {
		v, err := strconv.Atoi(p.Args[0])
		if err != nil || v < 1 || v > 1024 {
			return fmt.Errorf("bad GOROUTINES %q", p.Args[0])
		}
		n = v
	}
	if len(p.Args) > 1 {
		v, err := strconv.ParseFloat(p.Args[1], 64)
		if err != nil || v <= 0 {
			return fmt.Errorf("bad SECONDS %q", p.Args[1])
		}
		secs = v
	}
	run, cancel := context.WithTimeout(ctx, time.Duration(secs*float64(time.Second)))
	defer cancel()
	var bytes atomic.Int64
	done := make(chan struct{}, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		p.Go(func(context.Context) { // run ends with the Proc's ctx too
			defer func() { done <- struct{}{} }()
			buf := make([]byte, 64<<10)
			buf[0] = byte(i)
			for run.Err() == nil {
				s := sha256.Sum256(buf)
				copy(buf, s[:])
				bytes.Add(int64(len(buf)))
				// TamaGo never preempts a goroutine (no sysmon, no async
				// preemption): a loop that neither blocks nor allocates
				// keeps its core, and the timers queued on it, forever.
				runtime.Gosched()
			}
		})
	}
	for i := 0; i < n; i++ {
		<-done
	}
	el := time.Since(start).Seconds()
	_, err := fmt.Fprintf(p.Stdout, "burn: %d goroutines, %.1f MB/s\n", n, float64(bytes.Load())/el/1e6)
	return err
}
