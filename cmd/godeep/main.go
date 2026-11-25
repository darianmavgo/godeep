//go:build tamago

// Command godeep is the bare-metal kernel image (GOOS=tamago). The board is
// chosen by GOARCH: board_arm64.go (QEMU virt) or board_amd64.go (QEMU microvm).
package main

import (
	"context"
	"fmt"

	"godeep/kernel"
)

func main() {
	b := newBoard()
	err := kernel.Boot(context.Background(), b)
	fmt.Fprintf(b.Console.Out, "init exited: %v\n", err)
	select {} // nothing left to run; the Go scheduler idles
}
