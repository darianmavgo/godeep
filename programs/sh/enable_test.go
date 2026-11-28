package sh

import (
	"context"
	"errors"
	"strings"
	"testing"

	"godeep/kernel/dev"
	"godeep/kernel/proc"
	"godeep/kernel/tty"
)

type fakeFeature struct {
	name string
	on   bool
	err  error
}

func (f *fakeFeature) Name() string { return f.name }
func (f *fakeFeature) Help() string { return "turns " + f.name + " on" }
func (f *fakeFeature) State() string {
	if f.on {
		return "on"
	}
	return "off"
}
func (f *fakeFeature) Enable(context.Context) error {
	if f.err != nil {
		return f.err
	}
	f.on = true
	return nil
}

type fakeFeatures []dev.Feature

func (f fakeFeatures) Features() []dev.Feature { return f }

func TestEnable(t *testing.T) {
	smp := &fakeFeature{name: "smp"}
	agx := &fakeFeature{name: "agx", err: errors.New("no GPU")}
	dev.Register(dev.Driver{Compatible: "test,features", Run: func(ctx context.Context, d *dev.Device) error {
		d.Ready(fakeFeatures{smp, agx})
		<-ctx.Done()
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := dev.NewManager(proc.NewTable(), nil, nil)
	m.Start(ctx, []dev.Node{{Name: "features", Compatible: "test,features"}})
	if _, err := m.Get(ctx, "features"); err != nil {
		t.Fatal(err)
	}
	out := &syncBuf{}
	term := tty.New(make(chan byte), out, false)
	defer term.Close()
	env := Env{TTY: term, Dev: m}

	if err := enableCmd(ctx, env, nil, nil); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "smp") || !strings.Contains(s, "agx") || !strings.Contains(s, "off") {
		t.Fatalf("enable (list):\n%s", s)
	}
	if err := enableCmd(ctx, env, nil, []string{"smp"}); err != nil || !smp.on {
		t.Fatalf("enable smp: %v, on=%v", err, smp.on)
	}
	if err := enableCmd(ctx, env, nil, []string{"agx"}); err == nil || !strings.Contains(err.Error(), "no GPU") {
		t.Fatalf("enable agx: %v", err)
	}
	if err := enableCmd(ctx, env, nil, []string{"wifi"}); err == nil || !strings.Contains(err.Error(), "smp, agx") {
		t.Fatalf("enable wifi: %v", err)
	}
	env.NoPower = true
	if err := enableCmd(ctx, env, nil, []string{"smp"}); err == nil {
		t.Fatal("restricted enable succeeded")
	}
}

func TestEnableNoFeatures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := dev.NewManager(proc.NewTable(), nil, nil)
	m.Start(ctx, nil)
	out := &syncBuf{}
	term := tty.New(make(chan byte), out, false)
	defer term.Close()
	if err := enableCmd(ctx, Env{TTY: term, Dev: m}, nil, nil); err != nil || !strings.Contains(out.String(), "no optional features") {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
}
