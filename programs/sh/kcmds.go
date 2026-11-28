package sh

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"godeep/kernel/cap"
	"godeep/kernel/dev"
	"godeep/kernel/ipc"
	"godeep/kernel/proc"
)

func init() {
	builtins["restrict"] = builtin{"restrict [-vol A,B] [-ro] [-net none|dial] [-ports A,B] [-nodev] [-time DUR] [-goroutines N] [-out BYTES] [-mem BYTES] CMD ARGS...: run CMD with narrower capabilities and resource limits (sizes take K/M/G)", restrict}
	builtins["ports"] = builtin{"named ipc ports: type, servers, calls", ports}
	builtins["kvd"] = builtin{"kvd [NAME]: serve a key-value store on ipc port NAME (default kv); kvd & runs it in the background", kvd}
	builtins["kv"] = builtin{"kv [-p NAME] set K V | get K | del K | list: talk to a kvd", kv}
	builtins["top"] = builtin{"top [-n FRAMES] [-d SECONDS]: processes by sampled run time, goroutines and I/O (^C stops)", top}
}

// restrict runs a command with a narrowed copy of the shell's
// capabilities: a view of the namespace (some volumes, maybe read-only), a
// network without listen (dial) or none, some ipc ports, and no device
// authority (-nodev: no device manager, interrupts, DMA, halt or reboot).
// Narrowing is one-way: a restricted command
// that runs restrict again only narrows further.
func restrict(ctx context.Context, env Env, p *proc.Proc, args []string) error {
	usage := errors.New("usage: restrict [-vol A,B] [-ro] [-net none|dial] [-ports A,B] [-nodev] [-time DUR] [-goroutines N] [-out BYTES] [-mem BYTES] CMD ARGS...")
	var lim proc.Limits
	var vols, portNames []string
	ro, nodev, setVols, setPorts := false, false, false, false
	netMode := ""
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		flag := args[0]
		args = args[1:]
		val := func() (string, error) {
			if len(args) == 0 {
				return "", usage
			}
			v := args[0]
			args = args[1:]
			return v, nil
		}
		switch flag {
		case "-ro":
			ro = true
		case "-nodev":
			nodev = true
		case "-vol":
			v, err := val()
			if err != nil {
				return err
			}
			vols, setVols = splitList(v), true
		case "-ports":
			v, err := val()
			if err != nil {
				return err
			}
			portNames, setPorts = splitList(v), true
		case "-time", "-goroutines", "-out", "-mem":
			v, err := val()
			if err != nil {
				return err
			}
			if err := setLimit(&lim, flag, v); err != nil {
				return err
			}
		case "-net":
			v, err := val()
			if err != nil {
				return err
			}
			if v != "none" && v != "dial" {
				return usage
			}
			netMode = v
		default:
			return usage
		}
	}
	if len(args) == 0 {
		return usage
	}
	e := env
	if e.FS != nil && (setVols || ro) {
		if setVols && len(vols) == 0 {
			vols = []string{"\x00"} // -vol "": no volumes at all
		}
		e.FS = e.FS.View(vols, ro)
	}
	switch netMode {
	case "none":
		e.Net = nil
	case "dial":
		e.Net = cap.DialOnly(e.Net)
	}
	if setPorts {
		if e.Ports != nil {
			e.Ports = e.Ports.Only(portNames...)
		}
	}
	if nodev {
		e.Dev, e.IRQ, e.DMA, e.Halt, e.Kexec, e.NoPower = nil, nil, nil, nil, nil, true
	}
	e.Limits = e.Limits.Narrow(lim)
	return execute(ctx, e, p, &pipeline{stages: [][]string{args}})
}

func setLimit(l *proc.Limits, flag, v string) error {
	bad := fmt.Errorf("restrict %s %s: bad value", flag, v)
	switch flag {
	case "-time":
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return bad
		}
		l.Time = d
	case "-goroutines":
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return bad
		}
		l.Goroutines = n
	case "-out", "-mem":
		n, err := parseSize(v)
		if err != nil || n < 1 {
			return bad
		}
		if flag == "-out" {
			l.Output = n
		} else {
			l.Memory = n
		}
	}
	return nil
}

// parseSize reads 512, 64K, 16M, 1G (powers of 1024).
func parseSize(v string) (int64, error) {
	mult := int64(1)
	switch strings.ToUpper(v[len(v)-1:]) {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	}
	if mult != 1 {
		v = v[:len(v)-1]
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n * mult, err
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func ports(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	if env.Ports == nil {
		return errors.New("no ipc namespace")
	}
	w := table(env.out())
	fmt.Fprintln(w, "PORT\tTYPE\tSERVERS\tCALLS")
	for _, d := range env.Ports.List() {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\n", d.Name(), d.Type(), d.Servers(), d.Calls())
	}
	return w.Flush()
}

// KVReq is a request to a kvd port.
type KVReq struct {
	Op, Key, Value string // Op: set, get, del, list
}

// kvd serves an in-memory key-value store: the state belongs to the
// serving goroutine, clients reach it only through the port.
func kvd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	name := "kv"
	if len(args) == 1 {
		name = args[0]
	} else if len(args) > 1 {
		return errors.New("usage: kvd [NAME]")
	}
	if env.Ports == nil {
		return errors.New("no ipc namespace")
	}
	port := ipc.NewPort[KVReq, string](name)
	unpublish, err := env.Ports.Publish(port)
	if err != nil {
		return err
	}
	defer unpublish()
	store := map[string]string{}
	err = port.Serve(ctx, func(_ context.Context, r KVReq) (string, error) {
		switch r.Op {
		case "set":
			store[r.Key] = r.Value
			return "", nil
		case "get":
			v, ok := store[r.Key]
			if !ok {
				return "", fmt.Errorf("%s: no such key", r.Key)
			}
			return v, nil
		case "del":
			delete(store, r.Key)
			return "", nil
		case "list":
			keys := make([]string, 0, len(store))
			for k := range store {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var b strings.Builder
			for _, k := range keys {
				fmt.Fprintf(&b, "%s=%s\n", k, store[k])
			}
			return b.String(), nil
		}
		return "", fmt.Errorf("unknown op %q", r.Op)
	})
	if ctx.Err() != nil {
		return nil // killed: the normal end of a server
	}
	return err
}

func kv(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	name := "kv"
	if len(args) >= 2 && args[0] == "-p" {
		name, args = args[1], args[2:]
	}
	var req KVReq
	switch {
	case len(args) == 3 && args[0] == "set":
		req = KVReq{Op: "set", Key: args[1], Value: args[2]}
	case len(args) == 2 && (args[0] == "get" || args[0] == "del"):
		req = KVReq{Op: args[0], Key: args[1]}
	case len(args) == 1 && args[0] == "list":
		req = KVReq{Op: "list"}
	default:
		return errors.New("usage: kv [-p NAME] set K V | get K | del K | list")
	}
	port, err := ipc.Lookup[KVReq, string](env.Ports, name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := port.Call(ctx, req)
	if err != nil {
		return err
	}
	if req.Op == "get" {
		v += "\n"
	}
	_, err = fmt.Fprint(env.out(), v)
	return err
}

// top samples the goroutines every 50 ms and shows, per process, the share
// of samples in which one of its goroutines was running or runnable (with
// GOMAXPROCS=1, that is: wanting the CPU), next to its goroutines and the
// bytes through its stdin/stdout. Go has no per-goroutine CPU clock, and
// TamaGo no profiling timer, so sampling is the measure there is.
func top(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	frames, every := 0, time.Second
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return errors.New("usage: top [-n FRAMES] [-d SECONDS]")
		}
		switch args[i] {
		case "-n":
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 {
				return fmt.Errorf("bad -n %q", args[i+1])
			}
			frames = n
		case "-d":
			f, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || f < 0.1 {
				return fmt.Errorf("bad -d %q", args[i+1])
			}
			every = time.Duration(f * float64(time.Second))
		default:
			return errors.New("usage: top [-n FRAMES] [-d SECONDS]")
		}
		i++
	}
	var intr <-chan struct{}
	if env.TTY != nil && env.Out == nil {
		intr = env.TTY.Interrupts()
	}
	self := goid()
	const tick = 50 * time.Millisecond
	t := time.NewTicker(tick)
	defer t.Stop()
	load := cpuLoad(env)
	for frame := 1; frames == 0 || frame <= frames; frame++ {
		busy := map[int]int{}
		samples := 0
		var idle0 []time.Duration
		t0 := time.Now()
		if load != nil {
			idle0 = load.CPUIdle()
		}
		end := time.Now().Add(every)
		for time.Now().Before(end) {
			select {
			case <-t.C:
			case <-intr:
				return nil
			case <-ctx.Done():
				return nil
			}
			seen := map[int]bool{}
			for _, g := range proc.Snapshot() {
				if g.ID == self || (g.State != "running" && g.State != "runnable") {
					continue
				}
				if !seen[g.PID] {
					seen[g.PID] = true
					busy[g.PID]++
				}
			}
			samples++
		}
		cores := ""
		if load != nil {
			cores = coreLine(idle0, load.CPUIdle(), time.Since(t0))
		}
		if err := topFrame(env, busy, samples, cores); err != nil {
			return err
		}
	}
	return nil
}

// cpuLoad finds the board's per-core idle accounting among the devices.
func cpuLoad(env Env) dev.CPULoad {
	for _, s := range env.Dev.List() {
		if l, ok := s.Service.(dev.CPULoad); ok {
			return l
		}
	}
	return nil
}

// coreLine is each core's busy share over a frame: 1 - idle/elapsed.
func coreLine(a, b []time.Duration, el time.Duration) string {
	if el <= 0 || len(a) != len(b) {
		return ""
	}
	var sb strings.Builder
	total := 0.0
	for i := range a {
		busy := 1 - float64(b[i]-a[i])/float64(el)
		busy = min(1, max(0, busy))
		total += busy
		fmt.Fprintf(&sb, "  cpu%d %3.0f%%", i, 100*busy)
	}
	return fmt.Sprintf("cores: %d busy (%.1f of %d)%s\n", int(total+0.5), total, len(a), sb.String())
}

func topFrame(env Env, busy map[int]int, samples int, cores string) error {
	gs := proc.Snapshot()
	count := map[int]int{}
	for _, g := range gs {
		count[g.PID]++
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	procs := []*proc.Proc{}
	if env.Table != nil {
		procs = env.Table.List()
	}
	pct := func(pid int) float64 {
		if samples == 0 {
			return 0
		}
		return 100 * float64(busy[pid]) / float64(samples)
	}
	sort.SliceStable(procs, func(i, j int) bool { return pct(procs[i].PID) > pct(procs[j].PID) })
	fmt.Fprintf(env.out(), "\ntop - up %s, %d procs, %d goroutines, heap %s, %d GC\n",
		fmtDur(time.Since(env.Boot)), len(procs), len(gs), fmtBytes(m.HeapInuse), m.NumGC)
	fmt.Fprint(env.out(), cores)
	w := table(env.out())
	fmt.Fprintln(w, "PID\tPPID\tNAME\tSTATE\t%RUN\tGOROUTINES\tREAD\tWRITTEN\tUPTIME")
	fmt.Fprintf(w, "0\t-\t[kernel]\trunning\t%.1f\t%d\t-\t-\t%s\n", pct(0), count[0], fmtDur(time.Since(env.Boot)))
	fmt.Fprintf(w, "-\t-\t[go runtime]\t-\t%.1f\t%d\t-\t-\t-\n", pct(-1), count[-1])
	for _, p := range procs {
		in, out := p.IO()
		fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%.1f\t%d\t%s\t%s\t%s\n", p.PID, p.PPID, p.Name, p.State(),
			pct(p.PID), count[p.PID], fmtBytes(uint64(in)), fmtBytes(uint64(out)), fmtDur(time.Since(p.Started)))
	}
	return w.Flush()
}

// goid is the calling goroutine's ID (from its own stack header).
func goid() int {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	f := strings.Fields(string(buf))
	if len(f) >= 2 {
		id, _ := strconv.Atoi(f[1])
		return id
	}
	return -1
}
