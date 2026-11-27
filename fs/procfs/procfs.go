// Package procfs builds godeep's synthetic volumes: proc: (processes and
// the runtime) and sys: (devices), as fs/synth filesystems. They're plain
// files, so `cat proc:/1/status`, `ls sys:/`, pipelines, scp from /proc
// over SFTP and WASI programs all read them the same way.
package procfs

import (
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"godeep/fs/synth"
	"godeep/kernel/dev"
	"godeep/kernel/proc"
)

// Proc returns the proc: volume over a process table:
//
//	proc:/PID/status      name, pid, ppid, state, uptime, goroutines, I/O bytes
//	proc:/PID/cmdline     the name and arguments, one per line
//	proc:/PID/goroutines  the Proc's goroutines: id, state, function
//	proc:/uptime          seconds since boot
//	proc:/meminfo         Go heap and GC statistics
//	proc:/version         kernel version and Go runtime
func Proc(tab *proc.Table, boot time.Time, version string) *synth.FS {
	pids := &synth.Node{
		List: func() []string {
			var out []string
			for _, p := range tab.List() {
				out = append(out, strconv.Itoa(p.PID))
			}
			return out
		},
		Get: func(name string) *synth.Node {
			pid, err := strconv.Atoi(name)
			if err != nil || strconv.Itoa(pid) != name {
				return nil
			}
			p, ok := tab.Get(pid)
			if !ok {
				return nil
			}
			return procDir(p)
		},
	}
	fixed := map[string]*synth.Node{
		"uptime": synth.Text(func() string {
			return fmt.Sprintf("%.2f\n", time.Since(boot).Seconds())
		}),
		"meminfo": synth.Text(meminfo),
		"version": synth.Text(func() string {
			return fmt.Sprintf("godeep %s %s/%s %s GOMAXPROCS=%d\n", version,
				runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.GOMAXPROCS(0))
		}),
	}
	root := &synth.Node{
		List: func() []string {
			names := pids.List()
			for n := range fixed {
				names = append(names, n)
			}
			return names
		},
		Get: func(name string) *synth.Node {
			if n := fixed[name]; n != nil {
				return n
			}
			return pids.Get(name)
		},
	}
	return synth.New("procfs", "proc", root)
}

func procDir(p *proc.Proc) *synth.Node {
	return synth.Static(map[string]*synth.Node{
		"status": synth.Text(func() string {
			n := 0
			for _, g := range proc.Snapshot() {
				if g.PID == p.PID {
					n++
				}
			}
			in, out := p.IO()
			var b strings.Builder
			fmt.Fprintf(&b, "name\t%s\npid\t%d\nppid\t%d\nstate\t%s\n", p.Name, p.PID, p.PPID, p.State())
			fmt.Fprintf(&b, "uptime\t%s\ngoroutines\t%d\nread\t%d\nwritten\t%d\n",
				time.Since(p.Started).Truncate(time.Millisecond), n, in, out)
			return b.String()
		}),
		"cmdline": synth.Text(func() string {
			return strings.Join(append([]string{p.Name}, p.Args...), "\n") + "\n"
		}),
		"goroutines": synth.Text(func() string {
			var b strings.Builder
			for _, g := range proc.Snapshot() {
				if g.PID == p.PID {
					fmt.Fprintf(&b, "%d\t%s\t%s\n", g.ID, g.State, g.Where)
				}
			}
			return b.String()
		}),
	})
}

func meminfo() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	var b strings.Builder
	for _, kv := range []struct {
		k string
		v uint64
	}{
		{"HeapAlloc", m.HeapAlloc}, {"HeapInuse", m.HeapInuse}, {"HeapIdle", m.HeapIdle},
		{"HeapObjects", m.HeapObjects}, {"StackInuse", m.StackInuse}, {"Sys", m.Sys},
		{"TotalAlloc", m.TotalAlloc}, {"Mallocs", m.Mallocs}, {"Frees", m.Frees},
		{"NumGC", uint64(m.NumGC)}, {"PauseTotalNs", m.PauseTotalNs},
		{"Goroutines", uint64(runtime.NumGoroutine())},
	} {
		fmt.Fprintf(&b, "%s\t%d\n", kv.k, kv.v)
	}
	return b.String()
}

// Sys returns the sys: volume over the device manager, a directory per
// device:
//
//	sys:/NAME/compatible  the driver match
//	sys:/NAME/state       ready, starting, failed, …
//	sys:/NAME/pid         the driver's process (0: none)
//	sys:/NAME/restarts    how often the driver was restarted
//	sys:/NAME/irq, reg    interrupt lines, register windows
//	sys:/NAME/error       why it failed (empty if it didn't)
//	sys:/NAME/info        what the driver reports about itself (lsdev's INFO)
func Sys(mgr *dev.Manager) *synth.FS {
	find := func(name string) (dev.Status, bool) {
		for _, s := range mgr.List() {
			if s.Name == name {
				return s, true
			}
		}
		return dev.Status{}, false
	}
	field := func(name string, f func(dev.Status) string) *synth.Node {
		return synth.Text(func() string {
			s, ok := find(name)
			if !ok {
				return ""
			}
			if v := f(s); v != "" {
				return v + "\n"
			}
			return ""
		})
	}
	root := &synth.Node{
		List: func() []string {
			var out []string
			for _, s := range mgr.List() {
				out = append(out, s.Name)
			}
			return out
		},
		Get: func(name string) *synth.Node {
			if _, ok := find(name); !ok {
				return nil
			}
			return synth.Static(map[string]*synth.Node{
				"compatible": field(name, func(s dev.Status) string { return s.Compatible }),
				"state":      field(name, func(s dev.Status) string { return string(s.State) }),
				"pid":        field(name, func(s dev.Status) string { return strconv.Itoa(s.PID) }),
				"restarts":   field(name, func(s dev.Status) string { return strconv.Itoa(s.Restarts) }),
				"irq": field(name, func(s dev.Status) string {
					var out []string
					for _, i := range s.IRQ {
						out = append(out, strconv.Itoa(i))
					}
					return strings.Join(out, " ")
				}),
				"reg": field(name, func(s dev.Status) string {
					var out []string
					for _, r := range s.Reg {
						out = append(out, fmt.Sprintf("%#x+%#x", r.Base, r.Size))
					}
					return strings.Join(out, " ")
				}),
				"error": field(name, func(s dev.Status) string {
					if s.Err != nil {
						return s.Err.Error()
					}
					return ""
				}),
				"info": field(name, func(s dev.Status) string {
					if l, ok := s.Service.(dev.Lister); ok {
						return strings.Join(slices.Clone(l.Devices()), "\n")
					}
					return ""
				}),
			})
		},
	}
	return synth.New("sysfs", "sys", root)
}
