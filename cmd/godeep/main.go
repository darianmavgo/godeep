//go:build tamago

// Command godeep is the bare-metal kernel image (GOOS=tamago). The board is
// chosen by GOARCH: board_arm64.go (QEMU virt) or board_amd64.go (QEMU microvm).
package main

import (
	"context"
	"fmt"
	"runtime/debug"

	"godeep/kernel"
)

// noGC is set by `make ... NOGC=1` (-ldflags -X main.noGC=1).
var noGC string

func main() {
	if noGC != "" {
		debug.SetGCPercent(-1) // no collections, ever: the heap only grows
	}
	b := newBoard()
	if noGC != "" {
		b.Info += "; garbage collector off (NOGC=1)"
	}
	err := kernel.Boot(context.Background(), b)
	fmt.Fprintf(b.Console.Out, "init exited: %v\n", err)
	select {} // nothing left to run; the Go scheduler idles
}
