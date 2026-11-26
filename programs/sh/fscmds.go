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

	"godeep/kernel/block"
	"godeep/kernel/proc"
	"godeep/kernel/vfs"
)

// Files: a path names its volume, "nvme0p2:/docs/a.txt" or "ram:/x.wasm".
// Volumes come from the shell's namespace (env.FS, kernel/vfs): the RAM
// filesystem "ram" (writable) and every disk and partition, whose
// filesystem (FAT, ext2/3/4, read-only) is recognized when it's used.

func disks(env Env) map[string]block.Device { return vfs.Disks(env.Dev) }

func openFS(ctx context.Context, d block.Device) (vfs.Volume, error) { return vfs.OpenFS(ctx, d) }

func openPath(ctx context.Context, env Env, arg string) (fs.FS, string, error) {
	if env.FS == nil {
		return nil, "", errors.New("no filesystems")
	}
	v, path, err := env.FS.Resolve(ctx, arg)
	return v, path, err
}

// writable resolves VOLUME:PATH on a writable volume.
func writable(ctx context.Context, env Env, arg string) (vfs.Writable, string, error) {
	if env.FS == nil {
		return nil, "", errors.New("no filesystems")
	}
	v, path, err := env.FS.Resolve(ctx, arg)
	if err != nil {
		return nil, "", err
	}
	if !vfs.IsWritable(v) {
		return nil, "", fmt.Errorf("%s: %w", arg, vfs.ErrReadOnly)
	}
	return v.(vfs.Writable), path, nil
}

func rm(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: rm VOLUME:PATH...")
	}
	for _, a := range args {
		w, path, err := writable(ctx, env, a)
		if err != nil {
			return err
		}
		if err := w.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func mkdir(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mkdir VOLUME:PATH...")
	}
	for _, a := range args {
		w, path, err := writable(ctx, env, a)
		if err != nil {
			return err
		}
		if err := w.Mkdir(path); err != nil {
			return err
		}
	}
	return nil
}

func mv(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: mv VOLUME:FROM VOLUME:TO (same volume)")
	}
	fv, from, err := vfs.Split(args[0])
	if err != nil {
		return err
	}
	tv, to, err := vfs.Split(args[1])
	if err != nil {
		return err
	}
	if fv != tv {
		return errors.New("mv: across volumes: copy with cp, then rm")
	}
	w, _, err := writable(ctx, env, args[0])
	if err != nil {
		return err
	}
	return w.Rename(from, to)
}

func cp(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: cp VOLUME:FROM VOLUME:TO (e.g. cp nvme0p2:/hello.txt ram:/hello.txt)")
	}
	src, from, err := openPath(ctx, env, args[0])
	if err != nil {
		return err
	}
	data, err := fs.ReadFile(src, from)
	if err != nil {
		return err
	}
	w, to, err := writable(ctx, env, args[1])
	if err != nil {
		return err
	}
	return w.WriteFile(to, data)
}

// write VOLUME:PATH TEXT... writes the text (and a newline) to a file.
func write(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: write VOLUME:PATH TEXT...")
	}
	w, path, err := writable(ctx, env, args[0])
	if err != nil {
		return err
	}
	return w.WriteFile(path, []byte(strings.Join(args[1:], " ")+"\n"))
}

func df(ctx context.Context, env Env, _ *proc.Proc, _ []string) error {
	if env.FS == nil {
		return errors.New("no filesystems")
	}
	w := table(env.TTY)
	fmt.Fprintln(w, "VOLUME\tTYPE\tLABEL\tMODE")
	for _, name := range env.FS.Names(ctx) {
		v, err := env.FS.Open(ctx, name)
		if err != nil {
			continue
		}
		mode := "ro"
		if vfs.IsWritable(v) {
			mode = "rw"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, v.Type(), dash(v.Label()), mode)
	}
	return w.Flush()
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
		return errors.New("usage: ls VOLUME:PATH (e.g. ls nvme0p1:/, ls ram:/)")
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
		return errors.New("usage: cat VOLUME:PATH... (e.g. cat nvme0p2:/hello.txt)")
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
