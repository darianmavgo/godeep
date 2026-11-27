// Package vfs is the file namespace a Proc is given: named volumes, each an
// io/fs.FS, addressed as "VOLUME:PATH" (shell) or "/VOLUME/PATH" (SFTP).
//
// A namespace is a value passed to whoever may use it (sh.Env, sshd), not a
// global. Its volumes are those mounted into it (the RAM filesystem "ram")
// plus the volumes devices publish by name (a host share, "host"), plus
// every block volume the device manager has, a whole disk ("nvme0") or
// a partition ("nvme0p2"), whose filesystem (FAT, ext2/3/4, read-only) is
// recognized when it's opened.
package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"godeep/fs/ext4"
	"godeep/fs/fat"
	"godeep/kernel/block"
	"godeep/kernel/dev"
)

// Volume is a filesystem in the namespace.
type Volume interface {
	fs.FS
	Type() string
	Label() string
}

// Writable is a volume that can be changed (fs/memfs).
type Writable interface {
	Volume
	WriteFile(name string, data []byte) error
	Create(name string) (io.WriteCloser, error)
	WriteAt(name string, p []byte, off int64) (int, error)
	ReadAt(name string, p []byte, off int64) (int, error) // the live contents
	Truncate(name string, size int64) error
	Mkdir(name string) error
	Remove(name string) error
	Rename(from, to string) error
}

// Named is a volume a device publishes under its own name: a host share
// (drivers/virtio/p9, named by its tag, e.g. "host").
type Named interface {
	Volume
	VolumeName() string
}

// IsWritable reports whether v accepts writes now: it implements Writable,
// and (if it says) is open for writing: a FAT on a read-only disk has the
// methods but refuses.
func IsWritable(v Volume) bool {
	if _, ok := v.(Writable); !ok {
		return false
	}
	if w, ok := v.(interface{ Writable() bool }); ok {
		return w.Writable()
	}
	return true
}

// ErrReadOnly is returned for writes to a read-only volume.
var ErrReadOnly = errors.New("read-only filesystem")

// NS is a namespace.
type NS struct {
	ctx context.Context // the namespace's lifetime: opened volumes read with it
	dev *dev.Manager    // nil: no block volumes

	mu     sync.Mutex
	mounts map[string]Volume
	open   map[string]openVol // block volumes already opened

	// A view (View) narrows base: it sees only allow (nil: every volume),
	// read-only if ro, and can't mount or unmount.
	base  *NS
	allow map[string]bool
	ro    bool
}

// ErrPermission is returned by a view for what it withholds.
var ErrPermission = errors.New("not permitted in this namespace")

// View returns a narrowed namespace over n: only the volumes named in allow
// (nil or empty: all of them), every one read-only if readOnly. A view
// can't mount or unmount, and viewing a view only narrows it further: it is
// what a less trusted Proc is handed instead of the whole namespace.
func (n *NS) View(allow []string, readOnly bool) *NS {
	v := &NS{base: n, ro: readOnly}
	if len(allow) > 0 {
		v.allow = map[string]bool{}
		for _, a := range allow {
			v.allow[a] = true
		}
	}
	return v
}

// visible reports whether a view (and every view under it) shows name.
func (n *NS) visible(name string) bool {
	for v := n; v.base != nil; v = v.base {
		if v.allow != nil && !v.allow[name] {
			return false
		}
	}
	return true
}

func (n *NS) readOnly() bool {
	for v := n; v.base != nil; v = v.base {
		if v.ro {
			return true
		}
	}
	return false
}

func (n *NS) root() *NS {
	for n.base != nil {
		n = n.base
	}
	return n
}

// CanMount reports whether Mount and Unmount work here (not on a view).
func (n *NS) CanMount() bool { return n.base == nil }

// openVol is a block volume's filesystem, kept so every user shares one
// (two writers through separate FAT objects would corrupt it), with the
// disk it came from (a restarted driver means a new one).
type openVol struct {
	disk block.Device
	v    Volume
}

// New returns a namespace with the device manager's block volumes. ctx is
// its lifetime (the kernel's): volumes opened through it stay open, shared
// by every user, and do their I/O with it, not with the ctx of whichever
// command happened to open them first.
func New(ctx context.Context, mgr *dev.Manager) *NS {
	return &NS{ctx: ctx, dev: mgr, mounts: map[string]Volume{}, open: map[string]openVol{}}
}

// Context returns the namespace's lifetime context. Mounted volumes (such
// as TCP 9P shares) use it for background/later I/O so they outlive the
// command that mounted them.
func (n *NS) Context() context.Context { return n.root().ctx }

// Mount adds (or replaces) a volume under name. If an existing mounted
// volume is replaced and implements io.Closer, it is closed.
func (n *NS) Mount(name string, v Volume) {
	if !n.CanMount() {
		return // a view can't change the namespace (check CanMount)
	}
	n.mu.Lock()
	prev := n.mounts[name]
	n.mounts[name] = v
	n.mu.Unlock()
	if prev != nil && prev != v {
		if cl, ok := prev.(io.Closer); ok {
			cl.Close()
		}
	}
}

// Unmount removes a mounted volume and closes it if it implements io.Closer.
func (n *NS) Unmount(name string) error {
	if !n.CanMount() {
		return fmt.Errorf("umount %s: %w", name, ErrPermission)
	}
	n.mu.Lock()
	v, ok := n.mounts[name]
	if ok {
		delete(n.mounts, name)
	}
	n.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s: not a mounted volume", name)
	}
	if cl, ok := v.(io.Closer); ok {
		return cl.Close()
	}
	return nil
}

// Open returns the named volume.
func (n *NS) Open(ctx context.Context, name string) (Volume, error) {
	if n.base != nil {
		if !n.visible(name) {
			return nil, fmt.Errorf("%s: no such volume", name)
		}
		v, err := n.root().Open(ctx, name)
		if err != nil || !n.readOnly() {
			return v, err
		}
		return ReadOnly(v), nil
	}
	n.mu.Lock()
	v, ok := n.mounts[name]
	n.mu.Unlock()
	if ok {
		return v, nil
	}
	if n.dev == nil {
		return nil, fmt.Errorf("%s: no such volume", name)
	}
	if v := named(n.dev)[name]; v != nil {
		return v, nil
	}
	disk := diskOf(n.dev, name)
	n.mu.Lock()
	if o, ok := n.open[name]; ok && same(o.disk, disk) {
		n.mu.Unlock()
		return o.v, nil
	}
	n.mu.Unlock()
	d, err := BlockVolume(ctx, n.dev, name)
	if err != nil {
		return nil, err
	}
	f, err := OpenFS(n.ctx, d)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if o, ok := n.open[name]; ok && same(o.disk, disk) {
		return o.v, nil // opened concurrently: share that one
	}
	n.open[name] = openVol{disk: disk, v: f}
	return f, nil
}

// same reports whether two disk services are the same one (services that
// aren't comparable values never match: such a volume isn't cached).
func same(a, b block.Device) bool {
	if a == nil || b == nil || !reflect.TypeOf(a).Comparable() || !reflect.TypeOf(b).Comparable() {
		return false
	}
	return a == b
}

// diskOf is the disk service a volume name lives on ("nvme0p2" → nvme0).
func diskOf(mgr *dev.Manager, name string) block.Device {
	all := Disks(mgr)
	if d, ok := all[name]; ok {
		return d
	}
	if i := strings.LastIndexByte(name, 'p'); i > 0 {
		return all[name[:i]]
	}
	return nil
}

// Names lists the volumes that hold a filesystem: mounts, then disks and
// partitions with one godeep can read.
func (n *NS) Names(ctx context.Context) []string {
	if n.base != nil {
		var out []string
		for _, name := range n.root().Names(ctx) {
			if n.visible(name) {
				out = append(out, name)
			}
		}
		return out
	}
	seen := map[string]bool{}
	n.mu.Lock()
	var out []string
	for name := range n.mounts {
		seen[name] = true
		out = append(out, name)
	}
	n.mu.Unlock()
	sort.Strings(out)
	if n.dev == nil {
		return out
	}
	var shares []string
	for name := range named(n.dev) {
		if !seen[name] {
			seen[name] = true
			shares = append(shares, name)
		}
	}
	sort.Strings(shares)
	out = append(out, shares...)
	disks := Disks(n.dev)
	names := make([]string, 0, len(disks))
	for name := range disks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		d := disks[name]
		if !seen[name] {
			if _, err := OpenFS(ctx, d); err == nil {
				seen[name] = true
				out = append(out, name)
			}
		}
		parts, _, _ := block.Partitions(ctx, d)
		for _, p := range parts {
			pn := fmt.Sprintf("%sp%d", name, p.Index)
			if !seen[pn] {
				if _, err := OpenFS(ctx, p); err == nil {
					seen[pn] = true
					out = append(out, pn)
				}
			}
		}
	}
	return out
}

// Split turns "nvme0p2:/docs" into the volume and an io/fs path ("docs";
// "." for the root).
func Split(arg string) (vol, path string, err error) {
	vol, path, ok := strings.Cut(arg, ":")
	if !ok || vol == "" {
		return "", "", fmt.Errorf("%q: expected VOLUME:PATH, e.g. ram:/hello.wasm or nvme0p1:/docs", arg)
	}
	return vol, Clean(path), nil
}

// Clean turns a slash path ("/a/b/", "") into an io/fs path ("a/b", ".").
func Clean(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return "."
	}
	return p
}

// Resolve opens VOLUME:PATH's volume.
func (n *NS) Resolve(ctx context.Context, arg string) (Volume, string, error) {
	vol, path, err := Split(arg)
	if err != nil {
		return nil, "", err
	}
	v, err := n.Open(ctx, vol)
	if err != nil {
		return nil, "", err
	}
	return v, path, nil
}

// named returns the volumes devices publish, by name.
func named(mgr *dev.Manager) map[string]Volume {
	out := map[string]Volume{}
	for _, s := range mgr.List() {
		if v, ok := s.Service.(Named); ok && s.State == dev.Ready {
			out[v.VolumeName()] = v
		}
	}
	return out
}

// Disks returns the device manager's ready block devices, by name.
func Disks(mgr *dev.Manager) map[string]block.Device {
	out := map[string]block.Device{}
	for _, s := range mgr.List() {
		if d, ok := s.Service.(block.Device); ok && s.State == dev.Ready {
			out[s.Name] = d
		}
	}
	return out
}

// BlockVolume finds "nvme0" (a whole disk) or "nvme0p2" (its 2nd partition).
func BlockVolume(ctx context.Context, mgr *dev.Manager, name string) (block.Device, error) {
	all := Disks(mgr)
	if d, ok := all[name]; ok {
		return d, nil
	}
	if i := strings.LastIndexByte(name, 'p'); i > 0 {
		if d, ok := all[name[:i]]; ok {
			n, err := strconv.Atoi(name[i+1:])
			if err != nil {
				return nil, fmt.Errorf("%s: bad partition number", name)
			}
			parts, _, err := block.Partitions(ctx, d)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name[:i], err)
			}
			for _, p := range parts {
				if p.Index == n {
					return p, nil
				}
			}
			return nil, fmt.Errorf("%s: no partition %d", name[:i], n)
		}
	}
	return nil, fmt.Errorf("%s: no such volume (see lsblk)", name)
}

// OpenFS recognizes the filesystem on a block volume. FAT on a writable
// device opens for writing (fs/fat), ext2/3/4 is read-only.
func OpenFS(ctx context.Context, d block.Device) (Volume, error) {
	r := block.NewReader(ctx, d)
	switch {
	case ext4.Probe(r):
		f, err := ext4.New(r)
		if err != nil {
			return nil, err // not a typed nil in a non-nil Volume
		}
		return f, nil
	case fat.Probe(r):
		var f *fat.FS
		var err error
		if block.Writable(d) {
			f, err = fat.NewWritable(r, r)
		} else {
			f, err = fat.New(r)
		}
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	return nil, errors.New("no filesystem godeep can read (ext2/3/4, FAT)")
}

// ReadOnly returns v without its write methods (it no longer implements
// Writable, so every write path refuses it).
func ReadOnly(v Volume) Volume {
	if _, ok := v.(Writable); !ok {
		return v
	}
	return roVolume{v}
}

type roVolume struct{ v Volume }

func (r roVolume) Open(name string) (fs.File, error)          { return r.v.Open(name) }
func (r roVolume) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(r.v, name) }
func (r roVolume) Stat(name string) (fs.FileInfo, error)      { return fs.Stat(r.v, name) }
func (r roVolume) ReadFile(name string) ([]byte, error)       { return fs.ReadFile(r.v, name) }
func (r roVolume) Type() string                               { return r.v.Type() + ",ro" }
func (r roVolume) Label() string                              { return r.v.Label() }
