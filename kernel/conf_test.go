package kernel

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/proc"
	"godeep/kernel/vfs"
)

func TestParseConf(t *testing.T) {
	c := ParseConf(strings.NewReader("# x\nnet = 10.1.2.3/24\ndns=1.1.1.1\nrun = lsdev\nrun= ps\nbogus\nntp = off\nnet = nope\n"))
	if c.Net.Addr.String() != "10.1.2.3/24" || len(c.Net.DNS) != 1 || len(c.Run) != 2 || c.NTP != "off" || len(c.Errs) != 2 {
		t.Fatalf("%+v", c)
	}
}

// TestConfdRC: godeep/rc alone selects the volume and is sourced after
// godeep.conf's run lines.
func TestConfdRC(t *testing.T) {
	for _, withConf := range []bool{false, true} {
		img, err := block.MakeGPT(128<<20, 512, []block.PartSpec{
			{Type: block.TypeBasicData, Data: fatImage(t, "OTHER")},
			{Type: block.TypeBasicData, Data: fatImage(t, "GODEEPDATA")},
		})
		if err != nil {
			t.Fatal(err)
		}
		disk := &memDisk{bs: 512, data: img}
		compat := "test,rcdisk"
		if withConf {
			compat += "2"
		}
		dev.Register(dev.Driver{Compatible: compat, Run: func(ctx context.Context, d *dev.Device) error {
			d.Ready(disk)
			<-ctx.Done()
			return nil
		}})
		ctx, cancel := context.WithCancel(context.Background())
		tab := proc.NewTable()
		mgr := dev.NewManager(tab, nil, nil)
		mgr.Start(ctx, []dev.Node{{Name: "disk0", Compatible: compat}})
		if _, err := mgr.Get(ctx, "disk0"); err != nil {
			t.Fatal(err)
		}
		ns := vfs.New(ctx, mgr)
		v, err := ns.Open(ctx, "disk0p2")
		if err != nil {
			t.Fatal(err)
		}
		w := v.(vfs.Writable)
		w.Mkdir("godeep")
		if err := w.WriteFile(rcFile, []byte("echo from rc\n")); err != nil {
			t.Fatal(err)
		}
		if withConf {
			w.WriteFile(confFile, []byte("run = echo from conf\n"))
		}
		var mu sync.Mutex
		var ran []string
		c := &confd{label: "GODEEPDATA", mgr: mgr, fs: ns, log: io.Discard, run: func(_ context.Context, line string) {
			mu.Lock()
			ran = append(ran, line)
			mu.Unlock()
		}}
		c.start(ctx, tab)
		want := []string{"source disk0p2:/godeep/rc"}
		if withConf {
			want = append([]string{"echo from conf"}, want...)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			got := strings.Join(ran, "|")
			mu.Unlock()
			if got == strings.Join(want, "|") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("conf=%v: ran %q, want %q", withConf, got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}
}
