package procfs

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/proc"
)

type lister struct{}

func (lister) Devices() []string { return []string{"one", "two"} }

func TestProc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tab := proc.NewTable()
	p := tab.Spawn(ctx, 0, proc.Spec{Name: "sleeper", Args: []string{"a", "b"}, Main: func(ctx context.Context, p *proc.Proc) error {
		p.Go(func(ctx context.Context) { <-ctx.Done() })
		<-ctx.Done()
		return nil
	}})
	f := Proc(tab, time.Now(), "test")
	// Spawn returns before the Proc's goroutines start: wait for both.
	for deadline := time.Now().Add(5 * time.Second); ; {
		b, _ := fs.ReadFile(f, "1/goroutines")
		if strings.Count(string(b), "\n") == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %q", b)
		}
		time.Sleep(time.Millisecond)
	}
	// Not fstest: these files change between reads by design.
	ents, err := fs.ReadDir(f, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, " "); got != "1 meminfo uptime version" {
		t.Errorf("root lists %q", got)
	}
	if b, _ := fs.ReadFile(f, "meminfo"); !strings.Contains(string(b), "HeapAlloc\t") {
		t.Errorf("meminfo %q", b)
	}
	if b, _ := fs.ReadFile(f, "1/goroutines"); strings.Count(string(b), "\n") != 2 {
		t.Errorf("goroutines %q", b)
	}
	b, _ := fs.ReadFile(f, "1/status")
	for _, want := range []string{"name\tsleeper", "pid\t1", "state\trunning", "goroutines\t2"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("status lacks %q:\n%s", want, b)
		}
	}
	if b, _ := fs.ReadFile(f, "1/cmdline"); string(b) != "sleeper\na\nb\n" {
		t.Errorf("cmdline %q", b)
	}
	if _, err := f.Open("01/status"); err == nil {
		t.Error("01 resolves")
	}
	p.Kill()
	<-p.Done()
	if _, err := f.Open("1/status"); err == nil {
		t.Error("dead proc still listed")
	}
}

func TestSys(t *testing.T) {
	dev.Register(dev.Driver{Compatible: "procfs,test", Run: func(ctx context.Context, d *dev.Device) error {
		d.Ready(lister{})
		<-ctx.Done()
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := dev.NewManager(proc.NewTable(), nil, nil)
	m.Start(ctx, []dev.Node{{Name: "thing0", Compatible: "procfs,test", IRQ: []int{33}, Reg: []dev.Range{{Base: 0x1000, Size: 0x100}}},
		{Name: "orphan0", Compatible: "nobody,home"}})
	if _, err := m.Get(ctx, "thing0"); err != nil {
		t.Fatal(err)
	}
	f := Sys(m)
	if err := fstest.TestFS(f, "thing0/state", "thing0/info", "orphan0/state"); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		"thing0/state": "ready\n", "thing0/info": "one\ntwo\n", "thing0/irq": "33\n",
		"thing0/reg": "0x1000+0x100\n", "orphan0/state": "no driver\n", "thing0/compatible": "procfs,test\n",
	} {
		if b, _ := fs.ReadFile(f, file); string(b) != want {
			t.Errorf("%s = %q, want %q", file, b, want)
		}
	}
}
