// Package prog is the registry of programs compiled into the kernel image.
// Programs register from init(); the registry is read-only after boot.
package prog

import (
	"sort"

	"godeep/kernel/proc"
)

type Program struct {
	Name  string
	Usage string
	Main  proc.Main
}

var registry = map[string]Program{}

// Register adds a program. Call only from init().
func Register(p Program) { registry[p.Name] = p }

func Lookup(name string) (Program, bool) {
	p, ok := registry[name]
	return p, ok
}

func All() []Program {
	out := make([]Program, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
