package wasm

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"godeep/fs/memfs"
	"godeep/fs/ninep"
)

// A WASI program, built with the host's Go for GOOS=wasip1.
const testProgram = `package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
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
	case "env":
		fmt.Printf("env: FOO=%s PWD=%s TMPDIR=%s\n", os.Getenv("FOO"), os.Getenv("PWD"), os.Getenv("TMPDIR"))
		t0 := time.Now()
		time.Sleep(5 * time.Millisecond)
		fmt.Printf("clock: year=%d slept=%v\n", t0.Year(), time.Since(t0) >= 0)
	case "stdin":
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Printf("read: %q\n", line)
	case "write":
		base := "/ram"
		if len(os.Args) > 2 {
			base = os.Args[2]
		}
		os.MkdirAll(base+"/out", 0o755)
		err := os.WriteFile(base+"/out/result.txt", []byte("written by wasm\n"), 0o644)
		fmt.Println("write:", err)
		f, _ := os.OpenFile(base+"/out/result.txt", os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("appended\n")
		f.Close()
		b, err := os.ReadFile(base+"/out/result.txt")
		fmt.Printf("read back: %q %v\n", b, err)
		fmt.Println("rename:", os.Rename(base+"/out/result.txt", base+"/final.txt"))
		fmt.Println("remove dir:", os.Remove(base+"/out"))
		fmt.Println("readonly:", os.WriteFile("/vol/x", nil, 0o644) != nil)
		ents, _ := os.ReadDir(base)
		for _, e := range ents {
			fmt.Println("entry:", e.Name())
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
		"remove dir: <nil>", "readonly: true", "entry: final.txt"} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("missing %q: %q %v", want, out, err)
		}
	}
	if b, err := fs.ReadFile(testRAM, "final.txt"); err != nil || string(b) != "written by wasm\nappended\n" {
		t.Fatalf("host side: %q %v", b, err)
	}
}

func TestWritable9PVolume(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	a, b := net.Pipe()
	go ninep.Serve(a, root, false)
	t.Cleanup(func() { b.Close() })
	tr, err := ninep.DialTCP(context.Background(), func(context.Context, string, string) (net.Conn, error) { return b, nil }, "pipe", 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ninep.Mount(context.Background(), tr, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	out := &syncBuf{}
	err = Run(context.Background(), Program{
		Name: "t.wasm", Code: program(t), Args: []string{"write", "/dev"}, Stdout: out,
		Mounts: []Mount{
			{Path: "/vol", FS: fstest.MapFS{"greeting.txt": {Data: []byte("ro")}}},
			{Path: "/dev", FS: client},
		},
	})
	for _, want := range []string{"write: <nil>", `read back: "written by wasm\nappended\n" <nil>`, "rename: <nil>",
		"remove dir: <nil>", "readonly: true", "entry: final.txt"} {
		if err != nil || !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %q %v", want, out.String(), err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, "final.txt")); err != nil || string(got) != "written by wasm\nappended\n" {
		t.Fatalf("host disk: %q %v", got, err)
	}
}

func TestCacheAndEnv(t *testing.T) {
	cache := NewCache(2)
	defer cache.Close()
	code := program(t)

	// First run compiles (miss); second reuses cached module (hit).
	for i := 0; i < 2; i++ {
		out := &syncBuf{}
		err := Run(context.Background(), Program{
			Name: "t.wasm", Code: code, Args: []string{"env"},
			Env: []string{"FOO=bar"}, Stdout: out, Cache: cache,
			Mounts: []Mount{{Path: "/ram", FS: memfs.New("ram")}},
		})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		for _, want := range []string{"env: FOO=bar PWD=/ TMPDIR=/ram", "slept=true"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("run %d missing %q: %q", i, want, out.String())
			}
		}
	}
	hits, misses, cached := cache.Stats()
	if misses != 1 || hits != 1 || cached != 1 {
		t.Fatalf("cache stats: hits=%d misses=%d cached=%d, want 1/1/1", hits, misses, cached)
	}

	// Concurrent cached runs + cancellation of one doesn't break the cache.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := &syncBuf{}
			err := Run(context.Background(), Program{
				Name: "t.wasm", Code: code, Args: []string{"env"},
				Env: []string{"FOO=concurrent"}, Stdout: out, Cache: cache,
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	// Cancel a spinning run on the same shared cache.
	spinCtx, cancel := context.WithCancel(context.Background())
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	if err := Run(spinCtx, Program{Name: "t.wasm", Code: code, Args: []string{"spin"}, Stdout: &syncBuf{}, Cache: cache}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached spin cancel: %v", err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent cached run: %v", err)
	}
	// Cache still works after one module was killed.
	out := &syncBuf{}
	if err := Run(context.Background(), Program{
		Name: "t.wasm", Code: code, Args: []string{"env"},
		Env: []string{"FOO=after-kill"}, Stdout: out, Cache: cache,
	}); err != nil || !strings.Contains(out.String(), "FOO=after-kill") {
		t.Fatalf("after kill: %q %v", out.String(), err)
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
