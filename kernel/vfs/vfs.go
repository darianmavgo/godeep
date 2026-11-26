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

// ErrReadOnly is returned for writes to a read-only volume.
var ErrReadOnly = errors.New("read-only filesystem")

// NS is a namespace.
type NS struct {
	dev *dev.Manager // nil: no block volumes

	mu     sync.Mutex
	mounts map[string]Volume
}

// New returns a namespace with the device manager's block volumes.
func New(mgr *dev.Manager) *NS { return &NS{dev: mgr, mounts: map[string]Volume{}} }

// Mount adds a volume under name.
func (n *NS) Mount(name string, v Volume) {
	n.mu.Lock()
	n.mounts[name] = v
	n.mu.Unlock()
}

// Open returns the named volume.
func (n *NS) Open(ctx context.Context, name string) (Volume, error) {
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
	d, err := BlockVolume(ctx, n.dev, name)
	if err != nil {
		return nil, err
	}
	f, err := OpenFS(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return f, nil
}

// Names lists the volumes that hold a filesystem: mounts, then disks and
// partitions with one godeep can read.
func (n *NS) Names(ctx context.Context) []string {
	n.mu.Lock()
	var out []string
	for name := range n.mounts {
		out = append(out, name)
	}
	n.mu.Unlock()
	sort.Strings(out)
	if n.dev == nil {
		return out
	}
	var shares []string
	for name := range named(n.dev) {
		shares = append(shares, name)
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
		if _, err := OpenFS(ctx, d); err == nil {
			out = append(out, name)
		}
		parts, _, _ := block.Partitions(ctx, d)
		for _, p := range parts {
			if _, err := OpenFS(ctx, p); err == nil {
				out = append(out, fmt.Sprintf("%sp%d", name, p.Index))
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

// OpenFS recognizes the filesystem on a block volume.
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
		f, err := fat.New(r)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	return nil, errors.New("no filesystem godeep can read (ext2/3/4, FAT)")
}
