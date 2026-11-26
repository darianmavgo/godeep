package sh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"godeep/fs/ext4"
	"godeep/fs/fat"
	"godeep/kernel/block"
	"godeep/kernel/proc"
)

// Disks and filesystems, read-only. There is no mount table (no global
// namespace): a path names its disk or partition, "nvme0p2:/docs/a.txt",
// and the filesystem is recognized when it's used.

// disks returns the block devices the device manager has, by name.
func disks(env Env) map[string]block.Device {
	out := map[string]block.Device{}
	for _, s := range env.Dev.List() {
		if d, ok := s.Service.(block.Device); ok && s.State == "ready" {
			out[s.Name] = d
		}
	}
	return out
}

// volume finds "nvme0" (a whole disk) or "nvme0p2" (its 2nd partition).
func volume(ctx context.Context, env Env, name string) (block.Device, error) {
	all := disks(env)
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
	return nil, fmt.Errorf("%s: no such disk (see lsblk)", name)
}

// filesystem is a recognized filesystem on a volume.
type filesystem interface {
	fs.FS
	Type() string
	Label() string
}

func openFS(ctx context.Context, d block.Device) (filesystem, error) {
	r := block.NewReader(ctx, d)
	if ext4.Probe(r) {
		return ext4.New(r)
	}
	if fat.Probe(r) {
		return fat.New(r)
	}
	return nil, errors.New("no filesystem godeep can read (ext2/3/4, FAT)")
}

// splitPath turns "nvme0p2:/docs" into the volume and an io/fs path.
func splitPath(arg string) (vol, path string, err error) {
	vol, path, ok := strings.Cut(arg, ":")
	if !ok {
		return "", "", fmt.Errorf("%q: expected DISK:PATH, e.g. nvme0p1:/docs", arg)
	}
	path = strings.Trim(path, "/")
	if path == "" {
		path = "."
	}
	return vol, path, nil
}

func openPath(ctx context.Context, env Env, arg string) (fs.FS, string, error) {
	vol, path, err := splitPath(arg)
	if err != nil {
		return nil, "", err
	}
	d, err := volume(ctx, env, vol)
	if err != nil {
		return nil, "", err
	}
	f, err := openFS(ctx, d)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", vol, err)
	}
	return f, path, nil
}

func size(n int64) string {
	switch {
	case n >= 10<<30:
		return fmt.Sprintf("%dG", n>>30)
	case n >= 10<<20:
		return fmt.Sprintf("%dM", n>>20)
	case n >= 10<<10:
		return fmt.Sprintf("%dK", n>>10)
	}
	return strconv.FormatInt(n, 10)
}

func lsblk(ctx context.Context, env Env, _ *proc.Proc, _ []string) error {
	w := table(env.TTY)
	fmt.Fprintln(w, "NAME\tSIZE\tTYPE\tFS\tLABEL")
	all := disks(env)
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		d := all[n]
		parts, scheme, err := block.Partitions(ctx, d)
		kind := "disk"
		if err == nil {
			kind = "disk (" + scheme + ")"
		}
		fsType, label := "-", "-"
		if err != nil {
			if f, err := openFS(ctx, d); err == nil {
				fsType, label = f.Type(), f.Label()
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", n, size(block.Size(d)), kind, fsType, dash(label))
		for _, p := range parts {
			fsType, label := "-", p.Name
			if f, err := openFS(ctx, p); err == nil {
				fsType = f.Type()
				if f.Label() != "" {
					label = f.Label()
				}
			}
			fmt.Fprintf(w, "%sp%d\t%s\t%s\t%s\t%s\n", n, p.Index, size(block.Size(p)), p.Type, fsType, dash(label))
		}
	}
	return w.Flush()
}

func ls(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ls DISK:PATH (e.g. ls nvme0p1:/)")
	}
	f, path, err := openPath(ctx, env, args[0])
	if err != nil {
		return err
	}
	st, err := fs.Stat(f, path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		fmt.Fprintf(env.TTY, "%s %8s %s\n", st.Mode(), size(st.Size()), st.Name())
		return nil
	}
	ents, err := fs.ReadDir(f, path)
	if err != nil {
		return err
	}
	w := table(env.TTY)
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			return err
		}
		name := e.Name()
		switch {
		case e.IsDir():
			name += "/"
		case e.Type()&fs.ModeSymlink != 0:
			name += "@"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", info.Mode(), size(info.Size()), info.ModTime().UTC().Format("2006-01-02 15:04"), name)
	}
	return w.Flush()
}

func cat(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cat DISK:PATH... (e.g. cat nvme0p2:/hello.txt)")
	}
	for _, a := range args {
		f, path, err := openPath(ctx, env, a)
		if err != nil {
			return err
		}
		file, err := f.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(env.TTY, ctxReader{ctx, file})
		file.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ctxReader stops a long copy when the command is killed (^C).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
