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
	"godeep/fs/procfs"
	"godeep/kernel/cap"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/ipc"
	"godeep/kernel/irq"
	"godeep/kernel/klog"
	"godeep/kernel/proc"
	"godeep/kernel/pstore"
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
	// Size, if set, is the screen's size in characters (a framebuffer
	// console that is the only terminal): full-screen programs use it.
	Size func() (cols, rows int)
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
	// KexecCheck says whether Kexec could boot an image now; Kexec boots
	// it (after the kernel stopped everything) and returns only if it
	// can't. nil: no kexec on this board.
	KexecCheck func(image []byte) error
	Kexec      func(image []byte) error
	// Pstore is the console record kept across resets (the board copies
	// console and runtime output into it). nil: none.
	Pstore *pstore.Ring
	// BootLog and PrevBootLog are this boot's and the previous boot's
	// earliest steps, for boards that record them before the console
	// exists (board/applem1). Kept as ram:/bootlog.txt and
	// ram:/bootlog.prev.txt; the previous one also goes in the kernel log
	// (so logd saves it to the LogLabel volume). "": none.
	BootLog, PrevBootLog string
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
	if con.Size != nil {
		env.TTY.SetSize(con.Size())
	}
	env.FS.Mount("ram", memfs.New("ram"))
	env.FS.Mount("proc", procfs.Proc(tab, env.Boot, Version))
	env.FS.Mount("sys", procfs.Sys(mgr))
	env.Ports = ipc.NewNamespace()
	defer env.WASM.Close()
	defer env.TTY.Close()
	env.Log = klog.New(env.TTY, 0)
	env.Log.Record([]byte(banner))
	saveBootLogs(ctx, env, b)
	crash := b.Pstore.Crash()
	if crash != nil {
		if v, err := env.FS.Open(ctx, "ram"); err == nil {
			if w, ok := v.(vfs.Writable); ok {
				w.WriteFile(crashFile, crash)
			}
		}
		env.Log.Printf("pstore: the previous boot ended without a clean halt; its last %d bytes of console output are in ram:/%s", len(crash), crashFile)
	} else if b.Pstore != nil {
		env.Log.Printf("pstore: boot %d, %s", b.Pstore.Boots(), prevNote(b.Pstore))
	}
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
		saver = &logSaver{label: b.LogLabel, mgr: mgr, fs: env.FS, log: env.Log, crash: crash}
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
	var kexecImage []byte
	if b.Kexec != nil {
		env.Kexec = func(img []byte) error {
			if b.KexecCheck != nil {
				if err := b.KexecCheck(img); err != nil {
					return err
				}
			}
			haltOnce.Do(func() {
				kexecImage = img
				close(halt)
			})
			return nil
		}
	}
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
	if shutdown && kexecImage != nil {
		env.Log.Printf("godeep: kexec: booting a %d-byte image", len(kexecImage))
		if saver != nil {
			saver.flush()
		}
		b.Pstore.MarkClean()
		err := b.Kexec(kexecImage)
		env.Log.Printf("godeep: kexec failed: %v; powering off", err)
	}
	if shutdown {
		env.Log.Printf("godeep: halted")
		b.Pstore.MarkClean()
		if b.PowerOff != nil {
			b.PowerOff()
		}
	}
	if err := init.Err(); err != nil && err != proc.ErrKilled && err != context.Canceled {
		return err
	}
	return nil
}

// crashFile is where a crashed boot's last console output goes: in ram:
// and, through logd, on the data volume.
const crashFile = "crash.txt"

// saveBootLogs keeps the board's boot logs in ram: and logs the previous
// boot's.
func saveBootLogs(ctx context.Context, env sh.Env, b Board) {
	if b.BootLog == "" && b.PrevBootLog == "" {
		return
	}
	if v, err := env.FS.Open(ctx, "ram"); err == nil {
		if w, ok := v.(vfs.Writable); ok {
			if b.BootLog != "" {
				w.WriteFile("bootlog.txt", []byte(b.BootLog))
			}
			if b.PrevBootLog != "" {
				w.WriteFile("bootlog.prev.txt", []byte(b.PrevBootLog))
			}
		}
	}
	if b.PrevBootLog != "" {
		env.Log.Printf("bootlog: the previous boot's earliest steps (ram:/bootlog.prev.txt):\n%s", b.PrevBootLog)
	}
}

func prevNote(r *pstore.Ring) string {
	_, clean, ok := r.Previous()
	switch {
	case !ok:
		return "no record of a previous boot"
	case clean:
		return "the previous boot halted cleanly"
	}
	return "the previous boot left no output"
}

func clockOf(env sh.Env) *wallClock {
	c, _ := env.Clock.(*wallClock)
	return c
}
