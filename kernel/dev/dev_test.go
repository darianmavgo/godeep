package dev

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"godeep/kernel/proc"
)

var flakyRuns atomic.Int32

func init() {
	Register(Driver{Compatible: "test,base", Run: func(ctx context.Context, d *Device) error {
		time.Sleep(20 * time.Millisecond) // slow init
		d.Ready("base-service")
		<-ctx.Done()
		return nil
	}})
	Register(Driver{Compatible: "test,user", Run: func(ctx context.Context, d *Device) error {
		v, err := d.Lookup(ctx, "base")
		if err != nil {
			return err
		}
		d.Ready("user of " + v.(string))
		<-ctx.Done()
		return nil
	}})
	Register(Driver{Compatible: "test,flaky", Run: func(ctx context.Context, d *Device) error {
		if flakyRuns.Add(1) < 3 {
			d.Proc.Go(func(context.Context) { panic("driver bug") }) // crash in a driver goroutine
			<-ctx.Done()
			return nil
		}
		d.Ready("finally")
		<-ctx.Done()
		return nil
	}})
	Register(Driver{Compatible: "test,broken", Run: func(ctx context.Context, d *Device) error {
		return errors.New("hardware not responding")
	}})
	Register(Driver{Compatible: "test,probe", Run: func(ctx context.Context, d *Device) error {
		d.Ready([]string{"slot0"}) // probe-only: publish and exit
		return nil
	}})
}

func TestManager(t *testing.T) {
	flakyRuns.Store(0)
	tab := proc.NewTable()
	m := NewManager(tab, nil, nil)
	m.Backoff = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx, []Node{
		{Name: "user", Compatible: "test,user", Needs: []string{"base"}}, // listed before its dependency
		{Name: "base", Compatible: "test,base"},
		{Name: "flaky", Compatible: "test,flaky"},
		{Name: "broken", Compatible: "test,broken"},
		{Name: "probe", Compatible: "test,probe"},
		{Name: "dependent", Compatible: "test,user", Needs: []string{"broken"}},
		{Name: "mystery", Compatible: "acme,unknown"},
	})

	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	if v, err := m.Get(wctx, "user"); err != nil || v != "user of base-service" {
		t.Fatalf("user: %v %v", v, err)
	}
	if v, err := m.Get(wctx, "flaky"); err != nil || v != "finally" {
		t.Fatalf("flaky: %v %v", v, err)
	}
	if _, err := m.Get(wctx, "broken"); err == nil || !strings.Contains(err.Error(), "not responding") {
		t.Fatalf("broken: %v", err)
	}
	if _, err := m.Get(wctx, "dependent"); err == nil || !strings.Contains(err.Error(), "dependency broken") {
		t.Fatalf("dependent: %v", err)
	}
	if _, err := m.Get(wctx, "mystery"); err == nil || !strings.Contains(err.Error(), "no driver") {
		t.Fatalf("mystery: %v", err)
	}
	if _, err := m.Get(wctx, "nope"); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("nope: %v", err)
	}
	if v, err := m.Get(wctx, "probe"); err != nil || v.([]string)[0] != "slot0" {
		t.Fatalf("probe: %v %v", v, err)
	}

	byName := map[string]Status{}
	for _, s := range m.List() {
		byName[s.Name] = s
	}
	if s := byName["flaky"]; s.Restarts != 2 || s.State != Ready {
		t.Fatalf("flaky status: %+v", s)
	}
	if s := byName["broken"]; s.State != Failed || s.Restarts != 4 {
		t.Fatalf("broken status: %+v", s)
	}
	if s := byName["base"]; s.PID == 0 {
		t.Fatalf("base has no driver process: %+v", s)
	}

	cancel()
	grace, gc := context.WithTimeout(context.Background(), 2*time.Second)
	defer gc()
	if left := tab.Shutdown(grace); len(left) != 0 {
		t.Fatalf("processes left: %d", len(left))
	}
}
