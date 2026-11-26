// Package sh is the godeep shell.
package sh

import (
	"context"
	"errors"
	"fmt"
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
}

type builtin struct {
	usage string
	run   func(ctx context.Context, env Env, p *proc.Proc, args []string) error
}

var builtins map[string]builtin

func init() {
	builtins = map[string]builtin{
		"help":     {"list commands", help},
		"ps":       {"ps [-g] [PID]: processes, or goroutines with -g", ps},
		"kill":     {"kill PID...: cancel processes", kill},
		"uptime":   {"time since boot", uptime},
		"mem":      {"memory and scheduler stats", mem},
		"halt":     {"power off", func(context.Context, Env, *proc.Proc, []string) error { os.Exit(0); return nil }},
		"reboot":   {"reset the machine (needs a reboot device, e.g. a watchdog)", reboot},
		"exit":     {"exit this shell (init starts a new one)", nil},
		"lsdev":    {"lsdev [NAME...]: devices (or those whose name starts with NAME), drivers, state", lsdev},
		"irqstat":  {"interrupt lines and counts", irqstat},
		"irqlat":   {"irqlat [N]: measure interrupt-to-goroutine latency", irqlat},
		"dmastat":  {"DMA memory usage", dmastat},
		"date":     {"wall-clock time", date},
		"lsblk":    {"disks, partitions and their filesystems", lsblk},
		"evtest":   {"evtest [SECONDS]: input devices and their events (^C stops)", evtest},
		"ls":       {"ls VOLUME:PATH: list a directory (e.g. ls nvme0p1:/, ls ram:/)", ls},
		"cat":      {"cat VOLUME:PATH...: print files (e.g. cat nvme0p2:/hello.txt)", cat},
		"ifconfig": {"network interface: address, MAC, traffic", ifconfig},
		"pprof":    {"pprof NAME [1|2]: a runtime profile (goroutine, heap, allocs, threadcreate, block, mutex) as protobuf for `ssh godeep pprof heap > heap.pb.gz`; 1/2: as text", pprofCmd},
		"dmesg":    {"dmesg [-f]: the kernel log (-f: follow it until ^C)", dmesg},
		"host":     {"host NAME: look a name up (DNS)", hostCmd},
		"get":      {"get http://HOST/PATH: fetch a URL over HTTP, print the body", getCmd},
		"df":       {"volumes: ram, disks and partitions with a filesystem", df},
		"cp":       {"cp VOLUME:FROM VOLUME:TO: copy a file (to ram:)", cp},
		"mv":       {"mv VOLUME:FROM VOLUME:TO: rename in a writable volume", mv},
		"rm":       {"rm VOLUME:PATH...: remove files or empty directories (ram:)", rm},
		"mkdir":    {"mkdir VOLUME:PATH...: make directories (ram:)", mkdir},
		"write":    {"write VOLUME:PATH TEXT...: write a line to a file (ram:)", write},
	}
}

// Main returns the shell program. With arguments "-c LINE" it runs LINE and
// exits (what an SSH exec request runs); otherwise it reads commands from
// env.TTY.
func Main(env Env) proc.Main {
	return func(ctx context.Context, p *proc.Proc) error {
		t := env.TTY
		if len(p.Args) >= 1 && p.Args[0] == "-c" {
			args := strings.Fields(strings.Join(p.Args[1:], " "))
			if len(args) == 0 || args[0] == "exit" {
				return nil
			}
			err := run(ctx, env, p, args)
			if err != nil {
				fmt.Fprintf(t, "%s: %v\n", args[0], err)
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
			args := strings.Fields(line)
			if len(args) == 0 {
				continue
			}
			if args[0] == "exit" {
				return nil
			}
			if err := run(ctx, env, p, args); err != nil {
				fmt.Fprintf(t, "%s: %v\n", args[0], err)
			}
		}
	}
}

func run(ctx context.Context, env Env, p *proc.Proc, args []string) error {
	bg := false
	if args[len(args)-1] == "&" {
		bg, args = true, args[:len(args)-1]
		if len(args) == 0 {
			return nil
		}
	} else if strings.HasSuffix(args[len(args)-1], "&") {
		bg = true
		args[len(args)-1] = strings.TrimSuffix(args[len(args)-1], "&")
	}
	if b, ok := builtins[args[0]]; ok {
		return b.run(ctx, env, p, args[1:])
	}
	name, main := args[0], proc.Main(nil)
	if pg, ok := prog.Lookup(args[0]); ok {
		main = pg.Main
	} else if strings.HasSuffix(args[0], ".wasm") && strings.Contains(args[0], ":") {
		name, main = path.Base(args[0]), wasmMain(env, args[0])
	} else {
		return errors.New("command not found")
	}
	child := env.Table.Spawn(ctx, p.PID, proc.Spec{Name: name, Args: args[1:], Stdout: env.TTY, Main: main})
	if bg {
		fmt.Fprintf(env.TTY, "[%d] %s\n", child.PID, name)
		return nil
	}
	// Foreground: wait, and turn ^C on the console into a kill.
	for {
		select {
		case <-child.Done():
			if err := child.Err(); err != nil && !errors.Is(err, proc.ErrKilled) {
				return err
			}
			return nil
		case <-env.TTY.Interrupts():
			child.Kill()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func help(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	w := table(env.TTY)
	fmt.Fprintln(w, "BUILTIN\tDESCRIPTION")
	for _, n := range []string{"help", "ps", "kill", "uptime", "mem", "date", "lsdev", "irqstat", "irqlat", "dmastat", "lsblk", "df", "ls", "cat", "cp", "mv", "rm", "mkdir", "write", "ifconfig", "host", "get", "evtest", "exit", "halt", "reboot", "dmesg", "pprof"} {
		fmt.Fprintf(w, "%s\t%s\n", n, builtins[n].usage)
	}
	fmt.Fprintln(w, "\t")
	fmt.Fprintln(w, "PROGRAM\tUSAGE")
	for _, pg := range prog.All() {
		fmt.Fprintf(w, "%s\t%s\n", pg.Name, pg.Usage)
	}
	w.Flush()
	fmt.Fprintln(env.TTY, "\nappend & to run in the background; ^C kills the foreground program")
	fmt.Fprintln(env.TTY, "VOLUME:PATH.wasm ARGS... runs a WASI program (scp prog.wasm godeep: puts it in ram:)")
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
	fmt.Fprintf(env.TTY, "up %s\n", fmtDur(time.Since(env.Boot)))
	return nil
}

func mem(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w := table(env.TTY)
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
	n := env.Net
	if p, ok := n.(*cap.PendingNet); ok {
		var err error
		if n, err = p.Wait(ctx); err != nil {
			return err
		}
	}
	l, ok := n.(dev.Lister)
	if !ok {
		return errors.New("the network doesn't describe itself")
	}
	for _, line := range l.Devices() {
		fmt.Fprintf(env.TTY, "eth0: %s\n", line)
	}
	return nil
}

// wasmMain runs a WASI program from a volume: stdout to the terminal,
// stdin from it (line by line), every volume mounted read-only at /NAME.
func wasmMain(env Env, file string) proc.Main {
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
			Name: path.Base(file), Code: code, Args: p.Args,
			Stdin:  &wasm.LineReader{ReadLine: func() (string, error) { return env.TTY.ReadLine(ctx) }},
			Stdout: p.Stdout, Mounts: mounts,
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
		fmt.Fprintf(env.TTY, "(%d bytes of older log dropped)\n", n)
	}
	env.TTY.Write(env.Log.Bytes())
	if !follow {
		return nil
	}
	for {
		select {
		case line, ok := <-c:
			if !ok {
				return nil
			}
			env.TTY.Write(line)
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
	return p.WriteTo(env.TTY, debug)
}
