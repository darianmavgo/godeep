// Command godeep-hosted runs the godeep kernel as an ordinary host process.
package main

import (
	"context"
	"fmt"
	"os"

	"godeep/board/hosted"
	"godeep/kernel"
)

func main() {
	if err := kernel.Boot(context.Background(), hosted.NewConsole()); err != nil {
		fmt.Fprintln(os.Stderr, "godeep:", err)
		os.Exit(1)
	}
}
