package sh

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"godeep/kernel/input"
	"godeep/kernel/proc"
)

// evtest [SECONDS]: list input devices, then print their events until ^C
// (or for SECONDS).
func evtest(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: evtest [SECONDS]")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if len(args) == 1 {
		secs, err := strconv.ParseFloat(args[0], 64)
		if err != nil || secs <= 0 {
			return errors.New("usage: evtest [SECONDS]")
		}
		ctx, cancel = context.WithTimeout(ctx, time.Duration(secs*float64(time.Second)))
		defer cancel()
	}
	var names []string
	cases := []reflect.SelectCase{
		{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
		{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(env.TTY.Interrupts())},
	}
	for _, s := range env.Dev.List() {
		for _, src := range input.All(s.Service) {
			name := s.Name + "/" + src.InputKind()
			names = append(names, name)
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(src.Subscribe(ctx))})
		}
	}
	if len(names) == 0 {
		return errors.New("no input devices")
	}
	for _, n := range names {
		fmt.Fprintf(env.TTY, "input: %s\n", n)
	}
	fmt.Fprintln(env.TTY, "listening; ^C to stop")
	for {
		i, v, ok := reflect.Select(cases)
		switch {
		case i < 2:
			return nil // timeout, ^C, or killed
		case !ok:
			return nil // a source went away with ctx
		}
		fmt.Fprintf(env.TTY, "%s: %s\n", names[i-2], v.Interface().(input.Event))
	}
}
