// hello is a WASI program for godeep: build it for wasip1, copy it over SSH,
// run it from the shell. No kernel rebuild, no reboot.
//
//	make wasm && make push WASM=hello      (scp bin/wasm/hello.wasm godeep:)
//	godeep$ ram:/hello.wasm a b c          (or: ssh godeep ram:/hello.wasm a b c)
//
// Volumes are mounted read-only at /NAME: /ram, /nvme0p2...
package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

func main() {
	fmt.Printf("hello from WebAssembly on godeep (%s/%s, %s)\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Printf("args: %q\n", os.Args[1:])
	fmt.Printf("time: %s\n", time.Now().UTC().Format(time.RFC3339))
	ents, err := os.ReadDir("/")
	if err != nil {
		fmt.Println("volumes:", err)
		return
	}
	fmt.Print("volumes:")
	for _, e := range ents {
		fmt.Print(" /", e.Name())
	}
	fmt.Println()
	if b, err := os.ReadFile("/nvme0p2/hello.txt"); err == nil {
		fmt.Printf("/nvme0p2/hello.txt: %s", b)
	}
}
