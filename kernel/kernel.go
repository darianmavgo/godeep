// Package kernel boots godeep. It must never import board/*.
package kernel

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"godeep/fs/memfs"
	"godeep/kernel/cap"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/irq"
	"godeep/kernel/klog"
	"godeep/kernel/proc"
	"godeep/kernel/tty"
	"godeep/kernel/vfs"
	"godeep/net/netstack"
	_ "godeep/programs/coreutils"
	_ "godeep/programs/hello"
	"godeep/programs/initd"
	"godeep/programs/sh"
	"godeep/programs/wasm"
)

const Version = "0.1.0"

// How long processes get to exit after a shutdown request.
const shutdownGrace = 2 * time.Second

// Console is what a board provides: an input byte stream (fed by a driver
// goroutine) and an output writer.
type Console struct {
	In   <-chan byte
	Out  io.Writer
	Echo bool // the kernel must echo input (raw serial)
	// Input, if set, receives every event from every input device (mouse,
	// trackpad, keyboards): a framebuffer console's pointer, scrollback and
	// selection. nil: the console doesn't use pointing devices.
	Input InputHandler
}

// Board is everything the kernel needs from the platform.
type Board struct {
	// Info is printed under the boot banner (how the board booted).
	Info    string
	Console Console
	// Shutdown receives when the host asks godeep to power off: the VM's
	// power button (QEMU system_powerdown) or SIGTERM for the hosted build.
	// nil if the board has no such signal.
	Shutdown <-chan struct{}
	// PowerOff is called after a clean shutdown. nil: Boot just returns.
	PowerOff func()

	// IRQ routes hardware interrupts to driver goroutines (nil: none).
	IRQ *irq.Bridge
	// DMA allocates device-visible memory outside the Go heap (nil: none).
	DMA *dma.Allocator
	// Devices are started by the device manager, each driver as a process.
	Devices []dev.Node

	// Net is the board's own network (the hosted build: host sockets). nil:
	// the kernel runs its TCP/IP stack on the first network device the
	// device manager finds, configured with NetConfig.
	Net cap.Net
	// NetConfig is the stack's address setup. Zero: DHCP, falling back to
	// a link-local address (netstack.LinkLocal) while DHCP keeps trying.
	NetConfig netstack.Config
	// NTPServer is where ntpd gets the date ("": pool.ntp.org; "off": none).
	NTPServer string
	// LogLabel: save the kernel log to the writable FAT32 partition with
	// this label (godeep/log.txt; "": don't).
	LogLabel string
	// NetWait is how long netd waits for a network device at boot: 0 means
	// 10 s, negative forever (a USB Ethernet adapter plugged in later).
	NetWait time.Duration
}

// Boot starts PID 1 and blocks until it exits, ctx is done, or the board
// requests a shutdown. On shutdown every process is killed, given
// shutdownGrace to exit, and stragglers are reported.
func Boot(ctx context.Context, b Board) error {
	con := b.Console
	banner := fmt.Sprintf("godeep %s (%s/%s, %s, GOMAXPROCS=%d)\n",
		Version, runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.GOMAXPROCS(0))
	if b.Info != "" {
		banner += b.Info + "\n"
	}
	io.WriteString(con.Out, banner)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tab := proc.NewTable()
	mgr := dev.NewManager(tab, b.IRQ, b.DMA)
	mgr.Start(ctx, b.Devices)

	env := sh.Env{Table: tab, TTY: tty.New(con.In, con.Out, con.Echo), Boot: time.Now(),
		Dev: mgr, IRQ: b.IRQ, DMA: b.DMA, Net: b.Net, FS: vfs.New(ctx, mgr), WASM: wasm.NewCache(wasm.DefaultCacheSize)}
	env.FS.Mount("ram", memfs.New("ram"))
	defer env.WASM.Close()
	defer env.TTY.Close()
	env.Log = klog.New(env.TTY, 0)
	env.Log.Record([]byte(banner))
	if env.Net == nil {
		pn := startNet(ctx, tab, mgr, b.NetConfig, b.NetWait, env.Log)
		env.Net = pn
		clock := &wallClock{mgr: mgr, net: pn, kick: make(chan string, 1)}
		env.Clock = clock
		if server := b.NTPServer; server != "off" {
			if server == "" {
				server = "pool.ntp.org"
			}
			tab.Spawn(ctx, 0, proc.Spec{Name: "ntpd", Main: func(ctx context.Context, p *proc.Proc) error {
				clock.ntpd(ctx, server, env.Log)
				return nil
			}})
		}
	} else {
		env.Clock = &wallClock{mgr: mgr}
	}
	if con.Input != nil {
		startInput(ctx, tab, mgr, con.Input)
	}
	var saver *logSaver
	if b.LogLabel != "" {
		saver = &logSaver{label: b.LogLabel, mgr: mgr, fs: env.FS, log: env.Log}
		startLogd(ctx, tab, saver)
		cd := &confd{label: b.LogLabel, mgr: mgr, fs: env.FS, log: env.Log, clock: clockOf(env)}
		if pn, ok := env.Net.(*cap.PendingNet); ok {
			cd.net = pn
		}
		cd.run = func(ctx context.Context, line string) {
			s := tab.Spawn(ctx, 0, proc.Spec{Name: "sh", Args: []string{"-c", line}, Stdout: env.TTY, Main: sh.Main(env)})
			s.Wait(ctx)
		}
		cd.start(ctx, tab)
	}
	halt := make(chan struct{})
	var haltOnce sync.Once
	env.Halt = func() { haltOnce.Do(func() { close(halt) }) }
	init := env.Table.Spawn(ctx, 0, proc.Spec{Name: "init", Stdout: env.TTY, Main: initd.Main(env, Version)})

	var shutdown bool
	select {
	case <-init.Done():
	case <-ctx.Done():
	case <-b.Shutdown: // nil channel: never fires
		shutdown = true
		fmt.Fprintln(env.TTY)
		env.Log.Printf("godeep: power-off requested, stopping all processes")
	case <-halt:
		shutdown = true
		env.Log.Printf("godeep: halt, stopping all processes")
	}
	if shutdown && saver != nil {
		saver.flush() // while the disk's driver still runs
	}
	cancel()

	grace, done := context.WithTimeout(context.Background(), shutdownGrace)
	defer done()
	for _, p := range env.Table.Shutdown(grace) {
		env.Log.Printf("godeep: pid %d (%s) did not exit: %s", p.PID, p.Name, p.State())
	}
	if shutdown {
		env.Log.Printf("godeep: halted")
		if b.PowerOff != nil {
			b.PowerOff()
		}
	}
	if err := init.Err(); err != nil && err != proc.ErrKilled && err != context.Canceled {
		return err
	}
	return nil
}

func clockOf(env sh.Env) *wallClock {
	c, _ := env.Clock.(*wallClock)
	return c
}
