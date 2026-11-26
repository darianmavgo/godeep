// qemucheck boots godeep in QEMU (the command after --), types shell
// commands on the serial console and checks their output:
//
//	qemucheck -step 'lsblk=>godeep-ext4,FAT32' -step 'cat nvme0p2:/hello.txt=>hello from godeep' -- qemu-system-aarch64 ...
//
// Each step waits for the shell prompt, types the command, then waits for
// every expected string (comma-separated) in what the command printed. At
// the end it types "halt" and waits for QEMU to exit. The serial output is
// saved to -log.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type steps []string

func (s *steps) String() string     { return strings.Join(*s, "; ") }
func (s *steps) Set(v string) error { *s = append(*s, v); return nil }

const prompt = "godeep$ "

func main() {
	var st steps
	flag.Var(&st, "step", "COMMAND=>EXPECTED[,EXPECTED...] (repeatable)")
	timeout := flag.Duration("timeout", 60*time.Second, "per step (and for boot)")
	logPath := flag.String("log", "bin/qemucheck.log", "serial output")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: qemucheck -step 'cmd=>want' ... -- qemu args")
		os.Exit(2)
	}
	if err := run(st, flag.Args(), *timeout, *logPath); err != nil {
		fmt.Fprintln(os.Stderr, "qemucheck: FAIL:", err)
		fmt.Fprintln(os.Stderr, "  serial:", *logPath)
		os.Exit(1)
	}
}

func run(st steps, argv []string, timeout time.Duration, logPath string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	lf, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Process.Kill()

	text := make(chan string, 1) // the whole output so far, after each read
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		var all strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := out.Read(buf)
			if n > 0 {
				lf.Write(buf[:n])
				all.Write(buf[:n])
				select {
				case <-text:
				default:
				}
				text <- all.String()
			}
			if err != nil {
				return
			}
		}
	}()

	seen := ""
	// wait until s[from:] satisfies ok
	wait := func(what string, from int, ok func(string) bool) error {
		deadline := time.After(timeout)
		for !ok(seen[min(from, len(seen)):]) {
			select {
			case seen = <-text:
			case <-exited:
				return fmt.Errorf("QEMU exited while waiting for %s", what)
			case <-deadline:
				return fmt.Errorf("timed out after %v waiting for %s; last output:\n%s", timeout, what, tail(seen[min(from, len(seen)):]))
			}
		}
		return nil
	}
	if err := wait("the shell prompt", 0, func(s string) bool { return strings.Contains(s, prompt) }); err != nil {
		return err
	}
	for _, s := range st {
		command, want, _ := strings.Cut(s, "=>")
		from := len(seen)
		if _, err := io.WriteString(in, command+"\r"); err != nil {
			return err
		}
		// the command's output ends at the next prompt
		err := wait(fmt.Sprintf("%q to finish", command), from, func(s string) bool {
			i := strings.Index(s, command)
			return i >= 0 && strings.Contains(s[i+len(command):], prompt)
		})
		if err != nil {
			return err
		}
		got := seen[from:]
		for _, w := range strings.Split(want, ",") {
			if w != "" && !strings.Contains(got, w) {
				return fmt.Errorf("%q: output lacks %q:\n%s", command, w, tail(got))
			}
		}
		fmt.Printf("qemucheck: OK: %s\n", command)
	}
	io.WriteString(in, "halt\r")
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
	}
	return nil
}

func tail(s string) string {
	if len(s) > 3000 {
		s = "..." + s[len(s)-3000:]
	}
	return s
}
