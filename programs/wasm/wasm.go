// Package wasm runs WebAssembly programs (WASI preview 1, e.g. Go built
// with GOOS=wasip1 GOARCH=wasm) on wazero's interpreter: programs that are
// not compiled into the kernel, pushed over SSH (scp file.wasm godeep:) or
// mounted over 9P (host:/, mount9p) and run from the shell, with no reboot.
//
// A program runs as an ordinary Proc: ps shows it, kill and ^C stop it
// (wazero closes the module when the context ends, even mid-loop). Its
// stdout and stderr go to the terminal, stdin reads lines from it, and
// every volume is mounted at /VOLUME (/ram, /nvme0p1, /dev...): writable
// volumes (ram, writable FAT/9P) read-write, the rest read-only.
//
// wazero's compiler needs executable memory, which TamaGo doesn't hand out,
// so this is the interpreter: fine for tools, slower than native code.
// Passing a Cache reuses the wazero runtime and caches compiled modules by
// SHA-256 so repeated runs skip decoding, validation, and IR lowering.
package wasm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"testing/fstest"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental/sysfs"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"godeep/kernel/vfs"
)

// MemoryLimit caps a program's linear memory.
const MemoryLimit = 512 << 20

// DefaultCacheSize is the default number of compiled modules retained by Cache.
const DefaultCacheSize = 8

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
	Env    []string // "KEY=VALUE" entries
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer // nil: defaults to Stdout
	Mounts []Mount
	Cache  *Cache // optional: reuse runtime and compiled modules across runs
}

// ExitError is a nonzero exit status.
type ExitError struct{ Code uint32 }

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Cache holds a shared wazero interpreter runtime and an LRU cache of
// compiled modules keyed by SHA-256 of the Wasm binary. It is a capability
// value (not a global) owned by the kernel / shell environment.
type Cache struct {
	max    int
	hits   atomic.Uint64
	misses atomic.Uint64

	mu      sync.Mutex
	rt      wazero.Runtime
	entries map[[32]byte]*cacheEntry
	order   [][32]byte // oldest first
	closed  bool
}

type cacheEntry struct {
	mod     wazero.CompiledModule
	refs    int
	evicted bool
}

// NewCache returns a Cache keeping up to max compiled modules (DefaultCacheSize if <= 0).
// The underlying wazero runtime is created lazily on the first Run.
func NewCache(max int) *Cache {
	if max <= 0 {
		max = DefaultCacheSize
	}
	return &Cache{max: max, entries: map[[32]byte]*cacheEntry{}}
}

// Stats returns cache hits, misses, and the number of currently cached modules.
func (c *Cache) Stats() (hits, misses uint64, cached int) {
	c.mu.Lock()
	cached = len(c.entries)
	c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), cached
}

// Close closes the underlying runtime and all cached modules.
func (c *Cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	rt := c.rt
	c.rt = nil
	c.entries = nil
	c.order = nil
	c.mu.Unlock()
	if rt != nil {
		return rt.Close(context.Background())
	}
	return nil
}

func newRuntime(ctx context.Context) (wazero.Runtime, error) {
	rtc := wazero.NewRuntimeConfigInterpreter().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(MemoryLimit / 65536)
	rt := wazero.NewRuntimeWithConfig(ctx, rtc)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(context.Background())
		return nil, err
	}
	return rt, nil
}

func (c *Cache) acquire(ctx context.Context, code []byte) (wazero.Runtime, *cacheEntry, error) {
	key := sha256.Sum256(code)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, context.Canceled
	}
	if e, ok := c.entries[key]; ok {
		e.refs++
		c.touchLocked(key)
		rt := c.rt
		c.mu.Unlock()
		c.hits.Add(1)
		return rt, e, nil
	}
	if c.rt == nil {
		// Runtime outlives individual Procs; Cache.Close shuts it down.
		rt, err := newRuntime(context.Background())
		if err != nil {
			c.mu.Unlock()
			return nil, nil, err
		}
		c.rt = rt
	}
	rt := c.rt
	c.mu.Unlock()

	compiled, err := rt.CompileModule(ctx, code)
	if err != nil {
		return nil, nil, err
	}

	var toClose []wazero.CompiledModule
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		compiled.Close(context.Background())
		return nil, nil, context.Canceled
	}
	if e, ok := c.entries[key]; ok {
		// Compiled concurrently by another goroutine; reuse theirs.
		e.refs++
		c.touchLocked(key)
		c.mu.Unlock()
		compiled.Close(context.Background())
		c.hits.Add(1)
		return rt, e, nil
	}
	e := &cacheEntry{mod: compiled, refs: 1}
	c.entries[key] = e
	c.order = append(c.order, key)
	for len(c.order) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		if oe, ok := c.entries[oldest]; ok {
			delete(c.entries, oldest)
			oe.evicted = true
			if oe.refs == 0 {
				toClose = append(toClose, oe.mod)
			}
		}
	}
	c.mu.Unlock()
	for _, m := range toClose {
		m.Close(context.Background())
	}
	c.misses.Add(1)
	return rt, e, nil
}

func (c *Cache) touchLocked(key [32]byte) {
	for i, k := range c.order {
		if k == key {
			copy(c.order[i:], c.order[i+1:])
			c.order[len(c.order)-1] = key
			return
		}
	}
}

func (c *Cache) release(e *cacheEntry) {
	var mod wazero.CompiledModule
	c.mu.Lock()
	e.refs--
	if e.refs == 0 && e.evicted && !c.closed {
		mod = e.mod
	}
	c.mu.Unlock()
	if mod != nil {
		mod.Close(context.Background())
	}
}

// Run runs the program until it exits or ctx is done.
func Run(ctx context.Context, p Program) error {
	var (
		rt       wazero.Runtime
		compiled wazero.CompiledModule
		err      error
	)
	if p.Cache != nil {
		var entry *cacheEntry
		rt, entry, err = p.Cache.acquire(ctx, p.Code)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		defer p.Cache.release(entry)
		compiled = entry.mod
	} else {
		rt, err = newRuntime(ctx)
		if err != nil {
			return err
		}
		defer rt.Close(context.Background())
		compiled, err = rt.CompileModule(ctx, p.Code)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
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
	stderr := p.Stderr
	if stderr == nil {
		stderr = p.Stdout
	}
	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous: several runs may share a runtime
		WithArgs(append([]string{p.Name}, p.Args...)...).
		WithStdin(stdin).WithStdout(p.Stdout).WithStderr(stderr).
		WithFSConfig(fsc).
		WithSysWalltime().WithSysNanotime().WithSysNanosleep().
		WithRandSource(rand.Reader)
	seenEnv := map[string]bool{}
	for _, kv := range p.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			cfg = cfg.WithEnv(k, v)
			seenEnv[k] = true
		}
	}
	if !seenEnv["PWD"] {
		cfg = cfg.WithEnv("PWD", "/")
	}
	if !seenEnv["TMPDIR"] {
		if _, hasRAM := root["ram"]; hasRAM {
			cfg = cfg.WithEnv("TMPDIR", "/ram")
		}
	}
	mod, err := rt.InstantiateModule(ctx, compiled, cfg)
	if mod != nil {
		mod.Close(context.Background())
	}
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
