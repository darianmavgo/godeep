package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"strings"
	"testing"
	"time"
)

func countPID(pid int) (n int) {
	for _, g := range Snapshot() {
		if g.PID == pid {
			n++
		}
	}
	return
}

// eventually polls cond for up to 2s (goroutines finish unwinding after Done).
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestSpawnKillAttributesAndReapsGoroutines(t *testing.T) {
	tab := NewTable()
	ready := make(chan struct{}, 3)
	p := tab.Spawn(context.Background(), 0, Spec{Name: "t", Main: func(ctx context.Context, p *Proc) error {
		for i := 0; i < 3; i++ {
			p.Go(func(ctx context.Context) { ready <- struct{}{}; <-ctx.Done() })
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	for i := 0; i < 3; i++ {
		<-ready
	}
	if n := countPID(p.PID); n != 4 {
		t.Fatalf("want 4 goroutines labelled pid %d, got %d", p.PID, n)
	}
	if err := tab.Kill(p.PID); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(context.Background()); !errors.Is(err, ErrKilled) {
		t.Fatalf("want ErrKilled, got %v", err)
	}
	if _, ok := tab.Get(p.PID); ok {
		t.Fatal("proc still in table")
	}
	eventually(t, func() bool { return countPID(p.PID) == 0 }, "goroutines leaked")
}

func TestPanicKillsOnlyTheProc(t *testing.T) {
	tab := NewTable()
	p := tab.Spawn(context.Background(), 0, Spec{Name: "boom", Main: func(ctx context.Context, p *Proc) error {
		p.Go(func(context.Context) { panic("kaboom") })
		<-ctx.Done()
		return nil
	}})
	err := p.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("want panic error, got %v", err)
	}
}

func TestMainReturnEndsProc(t *testing.T) {
	tab := NewTable()
	p := tab.Spawn(context.Background(), 0, Spec{Name: "quick", Main: func(ctx context.Context, p *Proc) error {
		p.Go(func(ctx context.Context) { <-ctx.Done() })
		return nil
	}})
	if err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestParseStacks(t *testing.T) {
	in := `goroutine 1 [running]:
main.main()
	/x/main.go:21 +0x148

goroutine 19 [sleep, 2 minutes, locked to thread] {pid: 7, proc: spin}:
time.Sleep(0x34630b8a000)
	/go/src/runtime/time.go:368 +0x150
godeep/programs/coreutils.spin.func1()
	/x/spin.go:13 +0x28
created by godeep/programs/coreutils.spin in goroutine 1
	/x/spin.go:13 +0x24`
	gs := parseStacks(in)
	if len(gs) != 2 {
		t.Fatalf("got %d goroutines", len(gs))
	}
	g := gs[1]
	if g.ID != 19 || g.State != "sleep" || g.Waiting != "2 minutes" || !g.Locked || g.PID != 7 ||
		g.Labels["proc"] != "spin" || g.Func != "time.Sleep" ||
		g.Where != "godeep/programs/coreutils.spin.func1" || !strings.HasPrefix(g.CreatedBy, "godeep/programs/coreutils.spin") {
		t.Fatalf("bad parse: %+v", g)
	}
	if gs[0].PID != -1 {
		t.Fatalf("unlabelled goroutine got pid %d", gs[0].PID)
	}
}

// Spawning must not relabel the caller, and the child is attributed from birth.
func TestSpawnKeepsCallerLabels(t *testing.T) {
	tab := NewTable()
	var p *Proc
	for i := 0; i < 20; i++ {
		p = tab.Spawn(context.Background(), 0, Spec{Name: "c", Main: func(ctx context.Context, p *Proc) error {
			<-ctx.Done()
			return nil
		}})
		Kthread("k", func() {})
	}
	for _, g := range Snapshot() {
		if g.Where == "godeep/kernel/proc.TestSpawnKeepsCallerLabels" && g.PID != -1 {
			t.Fatalf("test goroutine was relabelled to pid %d", g.PID)
		}
	}
	tab.Shutdown(context.Background())
	_ = p
}

func TestLimits(t *testing.T) {
	tab := NewTable()
	ctx := context.Background()

	// Time: killed with ErrLimit.
	p := tab.Spawn(ctx, 0, Spec{Name: "slow", Limits: Limits{Time: 50 * time.Millisecond},
		Main: func(ctx context.Context, _ *Proc) error { <-ctx.Done(); return ctx.Err() }})
	p.Wait(ctx)
	if !errors.Is(p.Err(), ErrLimit) {
		t.Fatalf("time limit: %v", p.Err())
	}

	// Output: the write that crosses the limit is cut and the Proc killed.
	var out bytes.Buffer
	var mu sync.Mutex
	w := writerFunc(func(b []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return out.Write(b) })
	p = tab.Spawn(ctx, 0, Spec{Name: "chatty", Stdout: w, Limits: Limits{Output: 10},
		Main: func(ctx context.Context, p *Proc) error {
			for {
				if _, err := io.WriteString(p.Stdout, "abcd"); err != nil {
					return err
				}
			}
		}})
	p.Wait(ctx)
	if !errors.Is(p.Err(), ErrLimit) || out.String() != "abcdabcdab" {
		t.Fatalf("output limit: %v, wrote %q", p.Err(), out.String())
	}

	// Goroutines, inherited and narrowed by children.
	started := make(chan int, 1)
	parent := tab.Spawn(ctx, 0, Spec{Name: "parent", Limits: Limits{Goroutines: 3, Time: time.Hour},
		Main: func(ctx context.Context, p *Proc) error {
			n := 1
			for i := 0; i < 5; i++ {
				if p.Go(func(ctx context.Context) { <-ctx.Done() }) {
					n++
				}
			}
			child := tab.Spawn(ctx, p.PID, Spec{Name: "child", Limits: Limits{Goroutines: 10, Memory: 1 << 20},
				Main: func(ctx context.Context, c *Proc) error {
					if c.Limits != (Limits{Goroutines: 3, Time: time.Hour, Memory: 1 << 20}) {
						return fmt.Errorf("child limits %+v", c.Limits)
					}
					return nil
				}})
			child.Wait(ctx)
			started <- n
			if err := child.Err(); err != nil {
				return err
			}
			return nil
		}})
	if n := <-started; n != 3 {
		t.Fatalf("goroutines started: %d, want 3", n)
	}
	parent.Wait(ctx)
	if err := parent.Err(); err != nil {
		t.Fatal(err)
	}
	if s := (Limits{Time: time.Second, Output: 5}).String(); s != "time 1s, output 5 bytes" {
		t.Fatalf("String: %q", s)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
