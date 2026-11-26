// hot runs godeep in QEMU and reboots it into a fresh build whenever the
// source changes:
//
//	hot -build 'make -s image' -serial bin/hot-con.sock -qmp bin/qmp-arm64.sock -- qemu-system-aarch64 ...
//
// It watches the repository's .go and .s files (and go.mod, Makefile),
// skipping tests, bin/ and .git. On a change it rebuilds; if the build
// fails it prints the errors and leaves the running VM alone; if it
// succeeds it powers the VM off (QMP system_powerdown, so godeep shuts down
// cleanly; killed after a grace period) and boots the new image.
//
// The terminal stays attached across reboots: QEMU's serial port is a unix
// socket (the QEMU command must use -chardev socket,path=SERIAL,server=on,
// wait=on), and hot copies it to and from the terminal in raw mode, so ^C
// goes to godeep. Ctrl-A then: x quits, r rebuilds and reboots now, Ctrl-A
// sends a Ctrl-A.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	buildCmd   = flag.String("build", "make -s image", "build command (sh -c)")
	serialPath = flag.String("serial", "bin/hot-con.sock", "QEMU serial socket (server=on,wait=on)")
	qmpPath    = flag.String("qmp", "", "QEMU QMP socket, for a clean power-off")
	interval   = flag.Duration("interval", 300*time.Millisecond, "how often to check for changes")
	grace      = flag.Duration("grace", 4*time.Second, "how long godeep gets to shut down")
)

// out writes to the raw-mode terminal: \n needs a \r.
func say(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	os.Stdout.WriteString("\r\n\x1b[1;36m[hot]\x1b[0m " + strings.ReplaceAll(msg, "\n", "\r\n") + "\r\n")
}

func main() {
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: hot [flags] -- qemu-system-... (with a server=on,wait=on serial socket)")
		os.Exit(2)
	}
	if err := run(flag.Args()); err != nil {
		say("%v", err)
		os.Exit(1)
	}
}

func run(qemuArgs []string) error {
	restore, err := rawTerminal()
	if err != nil {
		return err
	}
	defer restore()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)

	keys := make(chan byte, 256)
	go func() { // the terminal's bytes
		b := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(b)
			for _, c := range b[:n] {
				keys <- c
			}
			if err != nil {
				close(keys)
				return
			}
		}
	}()

	say("watching for changes; Ctrl-A x quits, Ctrl-A r reboots into a fresh build")
	stamp := snapshot()
	if !build() {
		say("fix the build; waiting for changes")
		for !changed(&stamp) {
			select {
			case <-time.After(*interval):
			case <-sigs:
				return nil
			}
		}
		for !build() {
			for !changed(&stamp) {
				time.Sleep(*interval)
			}
		}
	}

	for {
		vm, err := boot(qemuArgs)
		if err != nil {
			return err
		}
		action := attach(vm, keys, &stamp, sigs)
		vm.stop()
		if action == quit {
			say("stopped")
			return nil
		}
		// action == reload: the new image is already built
	}
}

type action int

const (
	quit action = iota
	reload
)

// attach connects the terminal to the VM until a quit, a successful
// rebuild, or the VM exiting (halt: then wait for the next change).
func attach(vm *vm, keys <-chan byte, stamp *map[string]time.Time, sigs <-chan os.Signal) action {
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	prefix := false
	exited := vm.exited
	for {
		select {
		case <-sigs:
			return quit
		case c, ok := <-keys:
			if !ok {
				return quit
			}
			if prefix {
				prefix = false
				switch c {
				case 'x', 'X':
					return quit
				case 'r', 'R':
					if build() {
						return reload
					}
					continue
				case 0x01:
				default:
					continue
				}
			} else if c == 0x01 {
				prefix = true
				continue
			}
			vm.send(c)
		case <-tick.C:
			if changed(stamp) {
				say("change detected, rebuilding")
				if build() {
					return reload
				}
				say("build failed; godeep keeps running the previous image")
			}
		case <-exited:
			exited = nil
			say("QEMU exited (%v); waiting for a change, or Ctrl-A r", vm.err)
		}
	}
}

// ---- building and watching ----

func build() bool {
	start := time.Now()
	cmd := exec.Command("sh", "-c", *buildCmd)
	var outb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outb, &outb
	if err := cmd.Run(); err != nil {
		say("build failed (%v):\n%s", err, strings.TrimSpace(outb.String()))
		return false
	}
	say("built in %v", time.Since(start).Round(100*time.Millisecond))
	return true
}

// snapshot records the modification time of every watched file.
func snapshot() map[string]time.Time {
	m := map[string]time.Time{}
	filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		n := d.Name()
		watched := (strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go")) ||
			strings.HasSuffix(n, ".s") || n == "go.mod" || n == "Makefile"
		if watched {
			if info, err := d.Info(); err == nil {
				m[path] = info.ModTime()
			}
		}
		return nil
	})
	return m
}

// changed reports (and records) whether any watched file changed, was
// added or removed. It waits for edits to settle first.
func changed(stamp *map[string]time.Time) bool {
	now := snapshot()
	if sameFiles(*stamp, now) {
		return false
	}
	for { // let an editor's save (or a git checkout) finish
		time.Sleep(200 * time.Millisecond)
		next := snapshot()
		if sameFiles(now, next) {
			break
		}
		now = next
	}
	*stamp = now
	return true
}

func sameFiles(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !w.Equal(v) {
			return false
		}
	}
	return true
}

// ---- the VM ----

type vm struct {
	cmd    *exec.Cmd
	conn   net.Conn
	exited chan struct{}
	err    error
	stderr bytes.Buffer
}

func boot(args []string) (*vm, error) {
	os.Remove(*serialPath)
	v := &vm{exited: make(chan struct{})}
	v.cmd = exec.Command(args[0], args[1:]...)
	v.cmd.Stdout = io.Discard
	v.cmd.Stderr = &v.stderr
	if err := v.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		v.err = v.cmd.Wait()
		close(v.exited)
	}()
	// QEMU waits (wait=on) for the serial connection before running.
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.Dial("unix", *serialPath)
		if err == nil {
			v.conn = c
			break
		}
		select {
		case <-v.exited:
			return nil, fmt.Errorf("QEMU exited: %v\n%s", v.err, strings.TrimSpace(v.stderr.String()))
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			v.cmd.Process.Kill()
			return nil, errors.New("QEMU's serial socket never appeared")
		}
	}
	go io.Copy(os.Stdout, v.conn) // serial output (the guest sends \r\n)
	say("booted (pid %d)", v.cmd.Process.Pid)
	return v, nil
}

func (v *vm) send(c byte) { v.conn.Write([]byte{c}) }

// stop powers godeep off cleanly through QMP, then kills QEMU if needed.
func (v *vm) stop() {
	select {
	case <-v.exited:
		return
	default:
	}
	if *qmpPath != "" {
		if err := qmpPowerdown(*qmpPath); err == nil {
			select {
			case <-v.exited:
				v.conn.Close()
				return
			case <-time.After(*grace):
				say("godeep didn't power off in %v; killing QEMU", *grace)
			}
		}
	}
	v.cmd.Process.Kill()
	<-v.exited
	v.conn.Close()
}

func qmpPowerdown(path string) error {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	dec := json.NewDecoder(c)
	var greeting map[string]any
	if err := dec.Decode(&greeting); err != nil {
		return err
	}
	for _, cmd := range []string{"qmp_capabilities", "system_powerdown"} {
		if _, err := fmt.Fprintf(c, `{"execute":%q}`+"\n", cmd); err != nil {
			return err
		}
		for { // skip events until the command's reply
			var r map[string]any
			if err := dec.Decode(&r); err != nil {
				return err
			}
			if _, ok := r["return"]; ok {
				break
			}
			if e, ok := r["error"]; ok {
				return fmt.Errorf("qmp %s: %v", cmd, e)
			}
		}
	}
	return nil
}

// ---- terminal ----

// rawTerminal puts the terminal in raw mode (keys go to godeep unchanged,
// including ^C) and returns the function that restores it.
func rawTerminal() (func(), error) {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return func() {}, nil // not a terminal (e.g. piped): stay cooked
	}
	saved, err := stty(tty, "-g")
	if err != nil {
		tty.Close()
		return nil, err
	}
	if _, err := stty(tty, "raw", "-echo"); err != nil {
		tty.Close()
		return nil, err
	}
	return func() {
		stty(tty, strings.TrimSpace(saved))
		tty.Close()
	}, nil
}

func stty(tty *os.File, args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = tty
	out, err := cmd.Output()
	return string(out), err
}
