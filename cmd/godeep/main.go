//go:build tamago

// Command godeep is the bare-metal kernel image (GOOS=tamago).
package main

import (
	"context"
	"fmt"

	"godeep/board/microvm"
	"godeep/kernel"
)

func main() {
	con := microvm.NewConsole()
	err := kernel.Boot(context.Background(), con)
	fmt.Fprintf(con.Out, "init exited: %v\n", err)
	select {} // nothing left to run; the Go scheduler idles
}
