// Command pdcheck tests godeep on Parallels Desktop through the VM's serial
// port (a Unix socket, make parallels-setup) like tools/qemucheck does on
// QEMU: each step types a command and waits for text in the answer.
//
//	pdcheck -sock bin/parallels/serial.sock -vm GoDeep \
//	  -step 'cat proc:/version=>GOMAXPROCS=4' -step 'load:burn 4 10=>3.5'
//
// A "load:CMD=>N" step types CMD and, while it runs, samples the VM
// process's host CPU (ps %cpu): it passes when the average is at least N
// host cores.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type steps []string

func (s *steps) String() string     { return strings.Join(*s, ", ") }
func (s *steps) Set(v string) error { *s = append(*s, v); return nil }

var (
	conn net.Conn
	seen bytes.Buffer // everything received, scanned from mark
	mark int
)

func read(d time.Duration) {
	buf := make([]byte, 65536)
	conn.SetReadDeadline(time.Now().Add(d))
	n, _ := conn.Read(buf)
	seen.Write(bytes.ReplaceAll(buf[:n], []byte("\r"), nil))
}

// expect waits for want after the last mark.
func expect(want string, timeout time.Duration) bool {
	for end := time.Now().Add(timeout); time.Now().Before(end); {
		if i := bytes.Index(seen.Bytes()[mark:], []byte(want)); i >= 0 {
			mark += i + len(want)
			return true
		}
		read(200 * time.Millisecond)
	}
	return false
}

func send(cmd string) {
	mark = seen.Len()
	fmt.Fprintf(conn, "%s\r", cmd)
}

// vmPID finds the VM's process (prl_vm_app, named after the VM).
func vmPID(vm string) string {
	out, _ := exec.Command("pgrep", "-f", "prl_vm_app.*"+vm).Output()
	if f := strings.Fields(string(out)); len(f) > 0 {
		return f[0]
	}
	return ""
}

func cpu(pid string) float64 {
	out, _ := exec.Command("ps", "-o", "%cpu=", "-p", pid).Output()
	v, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return v / 100
}

func main() {
	sock := flag.String("sock", "bin/parallels/serial.sock", "the VM's serial port socket")
	vm := flag.String("vm", "GoDeep", "VM name (for load: steps)")
	boot := flag.Duration("boot", 60*time.Second, "how long to wait for the shell")
	var ss steps
	flag.Var(&ss, "step", "CMD=>WANT, or load:CMD=>CORES")
	flag.Parse()

	var err error
	for end := time.Now().Add(*boot); ; {
		if conn, err = net.Dial("unix", *sock); err == nil || time.Now().After(end) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		fail("connect: %v", err)
	}
	// the shell may already be waiting: ask for a prompt
	for end := time.Now().Add(*boot); !expect("godeep$", time.Second); {
		if time.Now().After(end) {
			fail("no shell prompt")
		}
		send("")
	}
	for _, s := range ss {
		cmd, want, _ := strings.Cut(s, "=>")
		if c, ok := strings.CutPrefix(cmd, "load:"); ok {
			min, _ := strconv.ParseFloat(want, 64)
			pid := vmPID(*vm)
			if pid == "" {
				fail("%s: no prl_vm_app process for VM %q", s, *vm)
			}
			send(c)
			time.Sleep(3 * time.Second)
			var sum float64
			for range 3 {
				sum += cpu(pid)
				time.Sleep(1500 * time.Millisecond)
			}
			avg := sum / 3
			if avg < min {
				fail("%s: VM used %.2f host cores, want >= %v", s, avg, min)
			}
			if !expect("godeep$", 60*time.Second) {
				fail("%s: no prompt after it", s)
			}
			fmt.Printf("pdcheck: OK: %s (%.2f host cores)\n", c, avg)
			continue
		}
		send(cmd)
		if !expect(want, 30*time.Second) {
			fail("%s: didn't see %q", cmd, want)
		}
		fmt.Printf("pdcheck: OK: %s\n", cmd)
	}
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "pdcheck: FAIL: "+f+"\n", a...)
	if t := seen.String(); len(t) > 0 {
		fmt.Fprintf(os.Stderr, "--- serial (last 2000 bytes) ---\n%s\n", t[max(0, len(t)-2000):])
	}
	os.Exit(1)
}
