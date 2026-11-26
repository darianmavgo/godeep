// Package wasm runs WebAssembly programs (WASI preview 1, e.g. Go built
// with GOOS=wasip1 GOARCH=wasm) on wazero's interpreter: programs that are
// not compiled into the kernel, pushed over SSH (scp file.wasm godeep:) and
// run from the shell (ram:/file.wasm ARGS), with no reboot.
//
// A program runs as an ordinary Proc: ps shows it, kill and ^C stop it
// (wazero closes the module when the context ends, even mid-loop). Its
// stdout and stderr go to the terminal, stdin reads lines from it, and
// every volume is mounted at /VOLUME (/ram, /nvme0p2...): writable volumes
// (ram) read-write, disks read-only.
//
// wazero's compiler needs executable memory, which TamaGo doesn't hand out,
// so this is the interpreter: fine for tools, slower than native code.
package wasm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing/fstest"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental/sysfs"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"godeep/kernel/vfs"
)

// MemoryLimit caps a program's linear memory.
const MemoryLimit = 512 << 20

// Mount is a filesystem visible to the program at Path.
type Mount struct {
	Path string // e.g. "/ram"
	FS   fs.FS
}

// Program is one run.
type Program struct {
	Name   string // argv[0]
	Code   []byte
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Mounts []Mount
}

// ExitError is a nonzero exit status.
type ExitError struct{ Code uint32 }

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Run runs the program until it exits or ctx is done.
func Run(ctx context.Context, p Program) error {
	rtc := wazero.NewRuntimeConfigInterpreter().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(MemoryLimit / 65536)
	rt := wazero.NewRuntimeWithConfig(ctx, rtc)
	defer rt.Close(context.Background())
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return err
	}
	compiled, err := rt.CompileModule(ctx, p.Code)
	if err != nil {
		return fmt.Errorf("%s: %w", p.Name, err)
	}
	// "/" lists the mounts as directories (paths under them resolve to the
	// mount, the longest matching prefix).
	root := fstest.MapFS{}
	fsc := wazero.NewFSConfig()
	for _, m := range p.Mounts {
		if v, ok := m.FS.(vfs.Volume); ok && vfs.IsWritable(v) {
			fsc = fsc.(sysfs.FSConfig).WithSysFSMount(&writableFS{w: v.(vfs.Writable)}, m.Path)
		} else if w, ok := m.FS.(vfs.Writable); ok && !isVolume(m.FS) {
			fsc = fsc.(sysfs.FSConfig).WithSysFSMount(&writableFS{w: w}, m.Path)
		} else {
			fsc = fsc.WithFSMount(m.FS, m.Path)
		}
		if name := strings.Trim(m.Path, "/"); name != "" && !strings.Contains(name, "/") {
			root[name] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}
		}
	}
	fsc = fsc.WithFSMount(root, "/")
	stdin := p.Stdin
	if stdin == nil {
		stdin = eof{}
	}
	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous: several runs may share a runtime later
		WithArgs(append([]string{p.Name}, p.Args...)...).
		WithStdin(stdin).WithStdout(p.Stdout).WithStderr(p.Stdout).
		WithFSConfig(fsc).
		WithSysWalltime().WithSysNanotime().WithSysNanosleep().
		WithRandSource(rand.Reader)
	_, err = rt.InstantiateModule(ctx, compiled, cfg)
	var exit *sys.ExitError
	if errors.As(err, &exit) {
		if ctx.Err() != nil {
			return ctx.Err() // closed because the Proc was killed
		}
		if exit.ExitCode() == 0 {
			return nil
		}
		return ExitError{exit.ExitCode()}
	}
	return err
}

type eof struct{}

func (eof) Read([]byte) (int, error) { return 0, io.EOF }

// LineReader turns a line source (a TTY's ReadLine) into stdin: each line
// ends with "\n"; the source's error (io.EOF on ^D) ends the input.
type LineReader struct {
	ReadLine func() (string, error)
	buf      []byte
	err      error
}

func (l *LineReader) Read(p []byte) (int, error) {
	for len(l.buf) == 0 {
		if l.err != nil {
			return 0, l.err
		}
		line, err := l.ReadLine()
		if err != nil {
			l.err = io.EOF
			continue
		}
		l.buf = []byte(line + "\n")
	}
	n := copy(p, l.buf)
	l.buf = l.buf[n:]
	return n, nil
}

func isVolume(f any) bool { _, ok := f.(vfs.Volume); return ok }
