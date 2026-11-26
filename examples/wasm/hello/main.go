// hello is a WASI program for godeep: build it for wasip1, copy it over SSH
// or mount it over 9P, and run it from the shell. No kernel rebuild, no reboot.
//
//	make wasm && make push WASM=hello      (scp bin/wasm/hello.wasm godeep:)
//	godeep$ ram:/hello.wasm a b c          (or: ssh godeep ram:/hello.wasm a b c)
//	godeep$ WRITE_FILE=/dev/hi.txt host:/bin/wasm/hello.wasm a b
//
// Every volume is mounted at /NAME: writable volumes (ram, writable FAT/9P)
// are read-write; the rest are read-only.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	fmt.Printf("hello from WebAssembly on godeep (%s/%s, %s)\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Printf("args: %q\n", os.Args[1:])
	fmt.Printf("time: %s\n", time.Now().UTC().Format(time.RFC3339))
	if msg := os.Getenv("GREETING"); msg != "" {
		fmt.Printf("env: GREETING=%s\n", msg)
	}
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
	if dst := os.Getenv("WRITE_FILE"); dst != "" {
		if dir := filepath.Dir(dst); dir != "." && dir != "/" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", dir, err)
				os.Exit(1)
			}
		}
		body := strings.Join(os.Args[1:], " ")
		if body == "" {
			body = "hello from wasm"
		}
		if err := os.WriteFile(dst, []byte(body+"\n"), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", dst, err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s\n", dst)
	}
}
