package sh

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"godeep/kernel/proc"
)

func table(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0) }

// ps lists processes with their goroutine counts and states, or with -g every
// goroutine with its PID, state, wait time and current function.
func ps(_ context.Context, env Env, _ *proc.Proc, args []string) error {
	goroutines, filter := false, -2
	for _, a := range args {
		if a == "-g" {
			goroutines = true
		} else if pid, err := strconv.Atoi(a); err == nil {
			filter = pid
			goroutines = true
		} else {
			return fmt.Errorf("usage: ps [-g] [PID]")
		}
	}
	gs := proc.Snapshot()
	if goroutines {
		return psGoroutines(env, gs, filter)
	}
	return psProcs(env, gs)
}

type tally struct {
	n      int
	states map[string]int
	name   string
}

func psProcs(env Env, gs []proc.Goroutine) error {
	by := map[int]*tally{}
	for _, g := range gs {
		t := by[g.PID]
		if t == nil {
			t = &tally{states: map[string]int{}}
			by[g.PID] = t
		}
		t.n++
		t.states[g.State]++
		if t.name == "" {
			t.name = g.Labels["proc"]
		}
	}

	w := table(env.out())
	fmt.Fprintln(w, "PID\tPPID\tNAME\tSTATE\tUPTIME\tGOROUTINES\tGOROUTINE STATES")
	row := func(pid, ppid, name, state, up string, t *tally) {
		n, st := 0, ""
		if t != nil {
			n, st = t.n, fmtStates(t.states)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", pid, ppid, name, state, up, n, st)
	}

	row("-", "-", "[go runtime]", "-", fmtDur(time.Since(env.Boot)), by[-1])
	if t := by[0]; t != nil {
		row("0", "-", "[kernel]", "running", fmtDur(time.Since(env.Boot)), t)
	}
	live := map[int]bool{-1: true, 0: true}
	for _, p := range env.Table.List() {
		live[p.PID] = true
		row(strconv.Itoa(p.PID), strconv.Itoa(p.PPID), p.Name, p.State(), fmtDur(time.Since(p.Started)), by[p.PID])
	}
	// Goroutines whose process is gone: kill is cooperative, so a goroutine
	// that ignores its context outlives the process. Show them.
	var orphans []int
	for pid := range by {
		if !live[pid] {
			orphans = append(orphans, pid)
		}
	}
	sort.Ints(orphans)
	for _, pid := range orphans {
		row(strconv.Itoa(pid), "?", by[pid].name, "orphaned", "-", by[pid])
	}
	return w.Flush()
}

func fmtStates(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d %s", m[k], k)
	}
	return strings.Join(parts, ", ")
}

func psGoroutines(env Env, gs []proc.Goroutine, filter int) error {
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].PID != gs[j].PID {
			return gs[i].PID < gs[j].PID
		}
		return gs[i].ID < gs[j].ID
	})
	w := table(env.out())
	fmt.Fprintln(w, "GID\tPID\tPROC\tSTATE\tWAITING\tFUNCTION")
	for _, g := range gs {
		if filter != -2 && g.PID != filter {
			continue
		}
		pid, name := "-", "[go runtime]"
		if g.PID >= 0 {
			pid, name = strconv.Itoa(g.PID), g.Labels["proc"]
		}
		st := g.State
		if g.Locked {
			st += " (locked)"
		}
		wait := g.Waiting
		if wait == "" {
			wait = "-"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", g.ID, pid, name, st, wait, shortFunc(g.Where))
	}
	return w.Flush()
}

// shortFunc trims the import path: godeep/programs/coreutils.spin.func1 -> coreutils.spin.func1
func shortFunc(f string) string {
	if i := strings.LastIndexByte(f, '/'); i >= 0 {
		return f[i+1:]
	}
	return f
}
