package sh

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"godeep/kernel/dev"
	"godeep/kernel/proc"
)

func lsdev(_ context.Context, env Env, _ *proc.Proc, args []string) error {
	w := table(env.TTY)
	fmt.Fprintln(w, "DEVICE\tCOMPATIBLE\tSTATE\tPID\tRESTARTS\tIRQ\tREG\tINFO")
	for _, s := range env.Dev.List() {
		if len(args) > 0 && !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(s.Name, a) }) {
			continue
		}
		pid := "-"
		if s.PID != 0 {
			pid = strconv.Itoa(s.PID)
		}
		var irqs, regs []string
		for _, i := range s.IRQ {
			irqs = append(irqs, strconv.Itoa(i))
		}
		for _, r := range s.Reg {
			regs = append(regs, fmt.Sprintf("%#x", r.Base))
		}
		info := ""
		switch {
		case s.Err != nil:
			info = s.Err.Error()
		case s.Service != nil:
			if l, ok := s.Service.(dev.Lister); ok {
				info = strings.Join(l.Devices(), ", ")
			} else if c, ok := s.Service.(dev.Clock); ok {
				info = c.Now().UTC().Format(time.RFC3339)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", s.Name, s.Compatible, s.State, pid, s.Restarts,
			dash(strings.Join(irqs, ",")), dash(strings.Join(regs, ",")), info)
	}
	return w.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func irqstat(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	if env.IRQ == nil {
		return errors.New("this board has no interrupt bridge")
	}
	lines, spurious := env.IRQ.Stats()
	w := table(env.TTY)
	fmt.Fprintln(w, "IRQ\tOWNER\tCOUNT\tAVG SERVICE\tMAX SERVICE")
	for _, l := range lines {
		fmt.Fprintf(w, "%d\t%s\t%d\t%v\t%v\n", l.ID, l.Name, l.Count, l.AvgDone, l.MaxDone)
	}
	fmt.Fprintf(w, "-\tunowned\t%d\t\t\n", spurious)
	return w.Flush()
}

func irqlat(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	n := 100
	if len(args) > 0 {
		v, err := strconv.Atoi(args[0])
		if err != nil || v < 1 {
			return errors.New("usage: irqlat [N]")
		}
		n = v
	}
	var probe dev.LatencyProbe
	for _, s := range env.Dev.List() {
		if p, ok := s.Service.(dev.LatencyProbe); ok {
			probe = p
		}
	}
	if probe == nil {
		return errors.New("no latency probe device on this board")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(n)*50*time.Millisecond+5*time.Second)
	defer cancel()
	d, err := probe.Measure(ctx, n)
	if err != nil {
		return err
	}
	slices.Sort(d)
	pct := func(p float64) time.Duration { return d[min(len(d)-1, int(p*float64(len(d))))] }
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	fmt.Fprintf(env.TTY, "timer deadline -> driver goroutine, %d samples:\n", len(d))
	fmt.Fprintf(env.TTY, "  min %v  median %v  avg %v  p99 %v  max %v\n",
		d[0], pct(0.5), sum/time.Duration(len(d)), pct(0.99), d[len(d)-1])
	return nil
}

func dmastat(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	if env.DMA == nil {
		return errors.New("this board has no DMA region")
	}
	s := env.DMA.Stats()
	w := table(env.TTY)
	fmt.Fprintf(w, "region\t%#x-%#x (%s)\n", s.Base, s.Base+uint64(s.Size), fmtBytes(uint64(s.Size)))
	fmt.Fprintf(w, "in use\t%s in %d buffers (peak %s)\n", fmtBytes(uint64(s.Used)), s.Buffers, fmtBytes(uint64(s.Peak)))
	fmt.Fprintf(w, "free spans\t%d, largest %s\n", s.FreeSpans, fmtBytes(uint64(s.LargestFreeSpan)))
	return w.Flush()
}

func date(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	now, src := time.Now(), "system clock"
	if env.Clock != nil {
		now, src = env.Clock.Now(), env.Clock.Source()
	}
	fmt.Fprintf(env.TTY, "%s (%s)\n", now.UTC().Format("Mon Jan 2 15:04:05 UTC 2006"), src)
	return nil
}

// wallNow is the date TLS checks certificates against.
func wallNow(env Env) func() time.Time {
	if env.Clock == nil {
		return time.Now
	}
	return env.Clock.Now
}

// ntp [SERVER]: sync the wall clock now.
func ntpCmd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if env.Clock == nil {
		return errors.New("no wall clock on this board")
	}
	server := "pool.ntp.org"
	if len(args) > 0 {
		server = args[0]
	}
	off, err := env.Clock.Sync(ctx, server)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.TTY, "clock corrected by %v: %s (%s)\n", off.Round(time.Millisecond),
		env.Clock.Now().UTC().Format("Mon Jan 2 15:04:05 UTC 2006"), env.Clock.Source())
	return nil
}

// kbdlight [PERCENT]: show or set the keyboard backlight.
func kbdlight(_ context.Context, env Env, _ *proc.Proc, args []string) error {
	type light interface {
		Set(int)
		Get() int
	}
	for _, s := range env.Dev.List() {
		l, ok := s.Service.(light)
		if !ok || s.State != dev.Ready {
			continue
		}
		if len(args) > 0 {
			n, err := strconv.Atoi(strings.TrimSuffix(args[0], "%"))
			if err != nil {
				return errors.New("usage: kbdlight [0-100]")
			}
			l.Set(n)
		}
		fmt.Fprintf(env.TTY, "keyboard backlight %d%%\n", l.Get())
		return nil
	}
	return errors.New("no keyboard backlight on this board")
}

func reboot(_ context.Context, env Env, _ *proc.Proc, _ []string) error {
	for _, s := range env.Dev.List() {
		if r, ok := s.Service.(dev.Rebooter); ok {
			fmt.Fprintf(env.TTY, "rebooting via %s\n", s.Name)
			r.Reboot()
			time.Sleep(time.Second)
			return errors.New("still running a second after the reset request")
		}
	}
	return errors.New("no reboot device on this board")
}
