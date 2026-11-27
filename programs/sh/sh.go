// Package sh is the godeep shell.
package sh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"godeep/kernel/cap"
	"godeep/kernel/dev"
	"godeep/kernel/dma"
	"godeep/kernel/ipc"
	"godeep/kernel/irq"
	"godeep/kernel/klog"
	"godeep/kernel/proc"
	"godeep/kernel/prog"
	"godeep/kernel/tty"
	"godeep/kernel/vfs"
	"godeep/programs/wasm"
)

// Env is what the shell is given. (Phase 2 replaces this with capabilities.)
type Env struct {
	Table *proc.Table
	TTY   *tty.TTY
	Boot  time.Time
	Dev   *dev.Manager
	IRQ   *irq.Bridge    // nil on boards without interrupts (hosted)
	DMA   *dma.Allocator // nil on boards without a DMA region
	Net   cap.Net        // nil if the board has no network
	FS    *vfs.NS        // volumes: ram, disks and partitions
	Log   *klog.Log      // the kernel log (dmesg); nil: none
	WASM  *wasm.Cache    // optional compiled-module cache for WASI programs
	// Clock is the wall clock (NTP, an RTC, or the system clock); nil: the
	// system clock.
	Clock WallClock
	// Out and In are a builtin's stdout and stdin when it runs as a stage
	// of a pipeline (or with a redirection); nil: the terminal.
	Out io.Writer
	In  io.Reader
	// Ports are the named ipc services this shell may find and offer.
	Ports *ipc.Namespace
	// NoPower: halt and reboot are refused (a restricted command).
	NoPower bool
	// Halt asks the kernel for a clean shutdown: every process (drivers
	// included) stops, then the board powers off. nil: halt exits at once.
	Halt func()
}

// WallClock is the kernel's date source.
type WallClock interface {
	Now() time.Time
	Source() string
	Sync(ctx context.Context, server string) (time.Duration, error)
}

type builtin struct {
	usage string
	run   func(ctx context.Context, env Env, p *proc.Proc, args []string) error
}

var builtins = map[string]builtin{} // filled from init()s

// halt shuts down cleanly when the kernel offers it: drivers finish (the
// Mac's SSD shuts its firmware down) before the board cuts power.
func halt(ctx context.Context, env Env, _ *proc.Proc, _ []string) error {
	if env.NoPower {
		return errors.New("not permitted")
	}
	if env.Halt == nil {
		os.Exit(0)
	}
	env.Halt()
	<-ctx.Done() // the shutdown kills this shell too
	return ctx.Err()
}

func init() {
	for name, b := range map[string]builtin{
		"help":     {"list commands", help},
		"ps":       {"ps [-g] [PID]: processes, or goroutines with -g", ps},
		"kill":     {"kill PID...: cancel processes", kill},
		"uptime":   {"time since boot", uptime},
		"mem":      {"memory and scheduler stats", mem},
		"halt":     {"power off (stops every process and driver first)", halt},
		"reboot":   {"reset the machine (needs a reboot device, e.g. a watchdog)", reboot},
		"exit":     {"exit this shell (init starts a new one)", nil},
		"lsdev":    {"lsdev [NAME...]: devices (or those whose name starts with NAME), drivers, state", lsdev},
		"irqstat":  {"interrupt lines and counts", irqstat},
		"irqlat":   {"irqlat [N]: measure interrupt-to-goroutine latency", irqlat},
		"dmastat":  {"DMA memory usage", dmastat},
		"date":     {"wall-clock time", date},
		"kbdlight": {"kbdlight [0-100]: show or set the keyboard backlight", kbdlight},
		"ntp":      {"ntp [SERVER]: set the date from an NTP server (default pool.ntp.org)", ntpCmd},
		"lsblk":    {"disks, partitions and their filesystems", lsblk},
		"evtest":   {"evtest [SECONDS]: input devices and their events (^C stops)", evtest},
		"ls":       {"ls VOLUME:PATH: list a directory (e.g. ls nvme0p1:/, ls ram:/)", ls},
		"cat":      {"cat [VOLUME:PATH...]: print files (e.g. cat nvme0p2:/hello.txt), or stdin", cat},
		"ifconfig": {"network interface: address, MAC, traffic", ifconfig},
		"pprof":    {"pprof NAME [1|2]: a runtime profile (goroutine, heap, allocs, threadcreate, block, mutex) as protobuf for `ssh godeep pprof heap > heap.pb.gz`; 1/2: as text", pprofCmd},
		"kbd":      {"kbd [LAYOUT]: keyboard layout for every keyboard (us, uk, dvorak, colemak)", kbd},
		"dmesg":    {"dmesg [-f]: the kernel log (-f: follow it until ^C)", dmesg},
		"host":     {"host NAME: look a name up (DNS)", hostCmd},
		"get":      {"get http[s]://HOST/PATH: fetch a URL, print the body", getCmd},
		"mount9p":  {"mount9p HOST:PORT NAME: mount a 9P2000.L TCP share (e.g. tools/ninepd)", mount9pCmd},
		"umount":   {"umount NAME: unmount a mounted volume", umount},
		"df":       {"volumes: ram, disks and partitions with a filesystem", df},
		"cp":       {"cp VOLUME:FROM VOLUME:TO: copy a file (to a writable volume)", cp},
		"mv":       {"mv VOLUME:FROM VOLUME:TO: rename in a writable volume", mv},
		"rm":       {"rm VOLUME:PATH...: remove files or empty directories (writable volume)", rm},
		"mkdir":    {"mkdir VOLUME:PATH...: make directories (writable volume)", mkdir},
		"write":    {"write VOLUME:PATH TEXT...: write a line to a file (writable volume)", write},
	} {
		builtins[name] = b
	}
}

// Main returns the shell program. With arguments "-c LINE" it runs LINE and
// exits (what an SSH exec request runs); otherwise it reads commands from
// env.TTY.
func Main(env Env) proc.Main {
	return func(ctx context.Context, p *proc.Proc) error {
		t := env.TTY
		if len(p.Args) >= 1 && p.Args[0] == "-c" {
			line := strings.Join(p.Args[1:], " ")
			err := runLine(ctx, env, p, line)
			if err != nil {
				fmt.Fprintf(t, "%s: %v\n", firstWord(line), err)
			}
			return err
		}
		fmt.Fprintln(t, "type 'help' for commands")
		for {
			fmt.Fprint(t, "godeep$ ")
			line, err := t.ReadLine(ctx)
			if errors.Is(err, tty.ErrInterrupt) {
				continue
			}
			if err != nil {
				return err
			}
			if w := firstWord(line); w == "exit" && len(strings.Fields(line)) == 1 {
				return nil
			}
			if err := runLine(ctx, env, p, line); err != nil {
				fmt.Fprintf(t, "%s: %v\n", firstWord(line), err)
			}
		}
	}
}

func firstWord(line string) string {
	if f := strings.Fields(line); len(f) > 0 {
		return f[0]
	}
	return "sh"
}

// out is where builtins print: their pipe or file, else the terminal.
func (e Env) out() io.Writer {
	if e.Out != nil {
		return e.Out
	}
	return e.TTY
}

func help(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	w := table(env.out())
	fmt.Fprintln(w, "BUILTIN\tDESCRIPTION")
	for _, n := range []string{"help", "ps", "kill", "uptime", "mem", "date", "ntp", "kbdlight", "lsdev", "irqstat", "irqlat", "dmastat", "lsblk", "df", "ls", "cat", "cp", "mv", "rm", "mkdir", "write", "mount9p", "umount", "ifconfig", "host", "get", "evtest", "exit", "halt", "reboot", "dmesg", "pprof", "kbd", "top", "restrict", "ports", "kvd", "kv", "gpu"} {
		fmt.Fprintf(w, "%s\t%s\n", n, builtins[n].usage)
	}
	fmt.Fprintln(w, "\t")
	fmt.Fprintln(w, "PROGRAM\tUSAGE")
	for _, pg := range prog.All() {
		fmt.Fprintf(w, "%s\t%s\n", pg.Name, pg.Usage)
	}
	w.Flush()
	fmt.Fprintln(env.out(), "\nA | B pipes A into B; < VOLUME:PATH reads, > / >> VOLUME:PATH writes / appends a file;")
	fmt.Fprintln(env.out(), "'quotes' keep blanks and | < > & in one word; append & to run in the background; ^C kills the foreground")
	fmt.Fprintln(env.out(), "VOLUME:PATH.wasm ARGS... runs a WASI program (scp prog.wasm godeep: puts it in ram:)")
	return nil
}

func kill(_ context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kill PID...")
	}
	for _, a := range args {
		pid, err := strconv.Atoi(a)
		if err != nil {
			return fmt.Errorf("bad pid %q", a)
		}
		if err := env.Table.Kill(pid); err != nil {
			return err
		}
	}
	return nil
}

func uptime(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	fmt.Fprintf(env.out(), "up %s\n", fmtDur(time.Since(env.Boot)))
	return nil
}

func mem(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w := table(env.out())
	fmt.Fprintf(w, "heap in use\t%s\n", fmtBytes(m.HeapInuse))
	fmt.Fprintf(w, "heap objects\t%d\n", m.HeapObjects)
	fmt.Fprintf(w, "total from system\t%s\n", fmtBytes(m.Sys))
	fmt.Fprintf(w, "GC cycles\t%d\n", m.NumGC)
	fmt.Fprintf(w, "goroutines\t%d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "GOMAXPROCS\t%d\n", runtime.GOMAXPROCS(0))
	return w.Flush()
}

func fmtBytes(b uint64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return d.Truncate(100 * time.Millisecond).String()
	}
	return d.Truncate(time.Second).String()
}

func ifconfig(ctx context.Context, env Env, _ *proc.Proc, _ []string) error {
	if env.Net == nil {
		return errors.New("no network on this board")
	}
	n, err := cap.Underlying(ctx, env.Net) // describing it needs no authority
	if err != nil {
		return err
	}
	l, ok := n.(dev.Lister)
	if !ok {
		return errors.New("the network doesn't describe itself")
	}
	for _, line := range l.Devices() {
		fmt.Fprintf(env.out(), "eth0: %s\n", line)
	}
	return nil
}

// isEnvAssign reports whether tok looks like KEY=VALUE with an identifier KEY.
func isEnvAssign(tok string) bool {
	k, _, ok := strings.Cut(tok, "=")
	if !ok || k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// wasmMain runs a WASI program from a volume: stdout to the terminal,
// stdin from it (line by line), every volume mounted at /NAME (writable
// volumes read-write, the rest read-only).
func wasmMain(env Env, file string, envVars []string) proc.Main {
	return func(ctx context.Context, p *proc.Proc) error {
		if env.FS == nil {
			return errors.New("no filesystems")
		}
		v, rel, err := env.FS.Resolve(ctx, file)
		if err != nil {
			return err
		}
		code, err := fs.ReadFile(v, rel)
		if err != nil {
			return err
		}
		var mounts []wasm.Mount
		for _, name := range env.FS.Names(ctx) {
			if vol, err := env.FS.Open(ctx, name); err == nil {
				mounts = append(mounts, wasm.Mount{Path: "/" + name, FS: vol})
			}
		}
		return wasm.Run(ctx, wasm.Program{
			Name: path.Base(file), Code: code, Args: p.Args, Env: envVars,
			Stdin:  p.Stdin,
			Stdout: p.Stdout, Mounts: mounts, Cache: env.WASM,
		})
	}
}

func dmesg(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if env.Log == nil {
		return errors.New("no kernel log")
	}
	follow := len(args) == 1 && args[0] == "-f"
	var c <-chan []byte
	if follow {
		c = env.Log.Follow(ctx) // before printing, so no line falls in between
	}
	if n := env.Log.Dropped(); n > 0 {
		fmt.Fprintf(env.out(), "(%d bytes of older log dropped)\n", n)
	}
	env.out().Write(env.Log.Bytes())
	if !follow {
		return nil
	}
	for {
		select {
		case line, ok := <-c:
			if !ok {
				return nil
			}
			env.out().Write(line)
		case <-env.TTY.Interrupts():
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// pprofCmd writes a runtime profile. Through ssh (no terminal) the
// protobuf goes out unchanged: ssh godeep pprof heap > heap.pb.gz, then
// go tool pprof heap.pb.gz on the host. (No CPU profile: TamaGo has no
// profiling timer signal.)
func pprofCmd(_ context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) == 0 || len(args) > 2 {
		var names []string
		for _, p := range pprof.Profiles() {
			names = append(names, p.Name())
		}
		return fmt.Errorf("usage: pprof NAME [1|2] (NAME: %s)", strings.Join(names, ", "))
	}
	p := pprof.Lookup(args[0])
	if p == nil {
		return fmt.Errorf("no profile %q", args[0])
	}
	debug := 0
	if len(args) == 2 {
		var err error
		if debug, err = strconv.Atoi(args[1]); err != nil {
			return fmt.Errorf("debug level %q", args[1])
		}
	}
	return p.WriteTo(env.out(), debug)
}
