package sh

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"godeep/kernel/dev"
	"godeep/kernel/proc"
)

// enable: features the board left off at boot (dev.Features), turned on
// from the shell. The Mac boots on one core without its GPU driver;
// `enable smp` releases the other cores, `enable agx` starts the GPU.

func init() {
	builtins["enable"] = builtin{"enable [FEATURE...]: list the board's optional features (smp, agx...), or turn them on", enableCmd}
}

func boardFeatures(env Env) []dev.Feature {
	if env.Dev == nil {
		return nil
	}
	var fs []dev.Feature
	for _, s := range env.Dev.List() {
		if f, ok := s.Service.(dev.Features); ok && s.State == dev.Ready {
			fs = append(fs, f.Features()...)
		}
	}
	return fs
}

func enableCmd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	fs := boardFeatures(env)
	if len(args) == 0 {
		if len(fs) == 0 {
			fmt.Fprintln(env.out(), "this board has no optional features")
			return nil
		}
		w := table(env.out())
		fmt.Fprintln(w, "FEATURE\tSTATE\tDESCRIPTION")
		for _, f := range fs {
			fmt.Fprintf(w, "%s\t%s\t%s\n", f.Name(), f.State(), f.Help())
		}
		return w.Flush()
	}
	if env.NoPower {
		return errors.New("not permitted")
	}
	var errs []error
	for _, a := range args {
		var f dev.Feature
		var names []string
		for _, x := range fs {
			names = append(names, x.Name())
			if x.Name() == a {
				f = x
			}
		}
		if f == nil {
			errs = append(errs, fmt.Errorf("%s: no such feature (this board has: %s)", a, dash(strings.Join(names, ", "))))
			continue
		}
		if err := f.Enable(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
			continue
		}
		fmt.Fprintf(env.out(), "%s: %s\n", a, f.State())
	}
	return errors.Join(errs...)
}
