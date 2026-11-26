// qemucheck boots godeep in QEMU (the command after --), types shell
// commands on the serial console and checks their output:
//
//	qemucheck -step 'lsblk=>godeep-ext4,FAT32' -step 'cat nvme0p2:/hello.txt=>hello from godeep' -- qemu-system-aarch64 ...
//
// Each step waits for the shell prompt, types the command, then waits for
// every expected string (comma-separated) in what the command printed. At
// the end it types "halt" and waits for QEMU to exit. The serial output is
// saved to -log.
//
// Other steps (QMP ones need -qmp, the socket QEMU was given):
//
//	send:CMD      type CMD on the serial console; don't wait for the prompt
//	wait:TEXT     wait until TEXT appears in output since the previous step
//	keys:TEXT     type TEXT on QEMU's keyboard (USB) through QMP; \r is Enter
//	qmp:JSON      send a QMP command, e.g. an input-send-event with mouse moves
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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
	qmpPath := flag.String("qmp", "", "QMP unix socket of the QEMU being run (for keys: and qmp: steps)")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: qemucheck -step 'cmd=>want' ... -- qemu args")
		os.Exit(2)
	}
	if err := run(st, flag.Args(), *timeout, *logPath, *qmpPath); err != nil {
		fmt.Fprintln(os.Stderr, "qemucheck: FAIL:", err)
		fmt.Fprintln(os.Stderr, "  serial:", *logPath)
		os.Exit(1)
	}
}

func run(st steps, argv []string, timeout time.Duration, logPath, qmpPath string) error {
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
	var q *qmp
	mark := len(seen)
	promptFrom := -1 // a send:/keys: step started a command: its prompt is still to come
	for _, s := range st {
		kind, arg, _ := strings.Cut(s, ":")
		switch kind {
		case "send":
			mark = len(seen)
			promptFrom = mark
			if _, err := io.WriteString(in, arg+"\r"); err != nil {
				return err
			}
			continue
		case "wait":
			if err := wait(fmt.Sprintf("%q", arg), mark, func(s string) bool { return strings.Contains(s, arg) }); err != nil {
				return err
			}
			fmt.Printf("qemucheck: OK: saw %q\n", arg)
			mark = len(seen)
			continue
		case "keys", "qmp":
			if q == nil {
				var err error
				if q, err = dialQMP(qmpPath); err != nil {
					return err
				}
				defer q.close()
			}
			mark = len(seen)
			if kind == "keys" {
				promptFrom = mark
			}
			var err error
			if kind == "keys" {
				err = q.keys(strings.ReplaceAll(arg, `\r`, "\r"))
			} else {
				err = q.do(arg)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
			fmt.Printf("qemucheck: OK: %s\n", s)
			continue
		}
		if promptFrom >= 0 { // the command a send:/keys: step started must be done
			if err := wait("the shell prompt", promptFrom, func(s string) bool { return strings.Contains(s, prompt) }); err != nil {
				return err
			}
			promptFrom = -1
		}
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
		mark = len(seen)
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

// qmp is a QEMU Machine Protocol connection.
type qmp struct {
	c net.Conn
	r *bufio.Reader
}

func dialQMP(path string) (*qmp, error) {
	if path == "" {
		return nil, errors.New("keys:/qmp: steps need -qmp")
	}
	var c net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if c, err = net.Dial("unix", path); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("QMP: %w", err)
	}
	q := &qmp{c: c, r: bufio.NewReader(c)}
	if _, err := q.r.ReadString('\n'); err != nil { // greeting
		return nil, err
	}
	return q, q.do(`{"execute":"qmp_capabilities"}`)
}

func (q *qmp) close() { q.c.Close() }

// do sends a command and waits for its answer (skipping events).
func (q *qmp) do(cmd string) error {
	q.c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(q.c, cmd+"\n"); err != nil {
		return err
	}
	for {
		line, err := q.r.ReadString('\n')
		if err != nil {
			return err
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if e, ok := m["error"]; ok {
			return fmt.Errorf("QMP error: %v", e)
		}
		if _, ok := m["return"]; ok {
			return nil
		}
	}
}

// keys types text with input-send-event: each key down then up, with
// shift for capitals.
func (q *qmp) keys(text string) error {
	for _, r := range text {
		code, shift := qcode(r)
		if code == "" {
			return fmt.Errorf("no QEMU key code for %q", r)
		}
		var evs []string
		key := func(c string, down bool) string {
			return fmt.Sprintf(`{"type":"key","data":{"down":%v,"key":{"type":"qcode","data":%q}}}`, down, c)
		}
		if shift {
			evs = append(evs, key("shift", true))
		}
		evs = append(evs, key(code, true), key(code, false))
		if shift {
			evs = append(evs, key("shift", false))
		}
		if err := q.do(`{"execute":"input-send-event","arguments":{"events":[` + strings.Join(evs, ",") + `]}}`); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond) // a few USB polls per key
	}
	return nil
}

func qcode(r rune) (code string, shift bool) {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return string(r), false
	case r >= 'A' && r <= 'Z':
		return string(r - 'A' + 'a'), true
	}
	switch r {
	case ' ':
		return "spc", false
	case '\r', '\n':
		return "ret", false
	case '-':
		return "minus", false
	case '.':
		return "dot", false
	case '/':
		return "slash", false
	case ':':
		return "semicolon", true
	}
	return "", false
}
