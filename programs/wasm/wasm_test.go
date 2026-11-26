package wasm

import (
	"io/fs"

	"bytes"
	"context"
	"errors"
	"godeep/fs/memfs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// A WASI program, built with the host's Go for GOOS=wasip1.
const testProgram = `package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func main() {
	fmt.Println("args:", strings.Join(os.Args[1:], ","))
	switch os.Args[1] {
	case "file":
		b, err := os.ReadFile("/vol/greeting.txt")
		fmt.Printf("file: %q %v\n", b, err)
		ents, err := os.ReadDir("/")
		for _, e := range ents {
			fmt.Printf("root: %s dir=%v\n", e.Name(), e.IsDir())
		}
		fmt.Println("root err:", err)
	case "stdin":
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Printf("read: %q\n", line)
	case "write":
		os.MkdirAll("/ram/out", 0o755)
		err := os.WriteFile("/ram/out/result.txt", []byte("written by wasm\n"), 0o644)
		fmt.Println("write:", err)
		f, _ := os.OpenFile("/ram/out/result.txt", os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("appended\n")
		f.Close()
		b, err := os.ReadFile("/ram/out/result.txt")
		fmt.Printf("read back: %q %v\n", b, err)
		fmt.Println("rename:", os.Rename("/ram/out/result.txt", "/ram/final.txt"))
		fmt.Println("remove dir:", os.Remove("/ram/out"))
		fmt.Println("readonly:", os.WriteFile("/vol/x", nil, 0o644) != nil)
		ents, _ := os.ReadDir("/ram")
		for _, e := range ents {
			fmt.Println("ram:", e.Name())
		}
	case "exit":
		os.Exit(3)
	case "spin":
		for i := 0; ; i++ {
			if i%100000000 == 0 {
				fmt.Println("spinning")
			}
		}
	}
}
`

var (
	buildOnce sync.Once
	code      []byte
	buildErr  error
)

func program(t *testing.T) []byte {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "wasmtest")
		if err != nil {
			buildErr = err
			return
		}
		defer os.RemoveAll(dir)
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module t\n\ngo 1.24\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "main.go"), []byte(testProgram), 0o644)
		cmd := exec.Command("go", "build", "-o", "t.wasm", ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			code = out
			return
		}
		code, buildErr = os.ReadFile(filepath.Join(dir, "t.wasm"))
	})
	if buildErr != nil {
		t.Skipf("can't build a wasip1 program: %v %s", buildErr, code)
	}
	return code
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func run(t *testing.T, ctx context.Context, args []string, stdin []string) (string, error) {
	out := &syncBuf{}
	lines := stdin
	err := Run(ctx, Program{
		Name: "t.wasm", Code: program(t), Args: args, Stdout: out,
		Stdin: &LineReader{ReadLine: func() (string, error) {
			if len(lines) == 0 {
				return "", errors.New("eof")
			}
			l := lines[0]
			lines = lines[1:]
			return l, nil
		}},
		Mounts: []Mount{
			{Path: "/vol", FS: fstest.MapFS{"greeting.txt": {Data: []byte("hi from a volume")}}},
			{Path: "/ram", FS: testRAM},
		},
	})
	return out.String(), err
}

var testRAM = memfs.New("ram")

func TestWritableVolume(t *testing.T) {
	out, err := run(t, context.Background(), []string{"write"}, nil)
	for _, want := range []string{"write: <nil>", `read back: "written by wasm\nappended\n" <nil>`, "rename: <nil>",
		"remove dir: <nil>", "readonly: true", "ram: final.txt"} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("missing %q: %q %v", want, out, err)
		}
	}
	if b, err := fs.ReadFile(testRAM, "final.txt"); err != nil || string(b) != "written by wasm\nappended\n" {
		t.Fatalf("host side: %q %v", b, err)
	}
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	if out, err := run(t, ctx, []string{"file", "x"}, nil); err != nil ||
		!strings.Contains(out, "args: file,x") || !strings.Contains(out, `file: "hi from a volume" <nil>`) ||
		!strings.Contains(out, "root: vol dir=true") || !strings.Contains(out, "root err: <nil>") {
		t.Fatalf("file: %q %v", out, err)
	}
	if out, err := run(t, ctx, []string{"stdin"}, []string{"typed line"}); err != nil || !strings.Contains(out, `read: "typed line\n"`) {
		t.Fatalf("stdin: %q %v", out, err)
	}
	if _, err := run(t, ctx, []string{"exit"}, nil); !errors.Is(err, ExitError{3}) {
		t.Fatalf("exit: %v", err)
	}
	if err := Run(ctx, Program{Name: "bad", Code: []byte("not wasm"), Stdout: &syncBuf{}}); err == nil {
		t.Fatal("garbage ran")
	}
}

func TestKill(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := run(t, ctx, []string{"spin"}, nil); done <- err }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("killed program: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a spinning program ignored cancellation")
	}
}
