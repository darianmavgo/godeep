package sh

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"godeep/kernel/proc"
	"godeep/programs/edit"
)

func init() {
	builtins["edit"] = builtin{"edit VOLUME:PATH: full-screen editor (^S save, ^Q quit, ^K/^U cut/paste lines, ^F find, ^G go to line)", editCmd}
}

// editCmd runs programs/edit on the terminal: raw input, ANSI output
// (understood by serial and SSH terminals and the framebuffer console).
func editCmd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: edit VOLUME:PATH")
	}
	if env.TTY == nil || env.In != nil || env.Out != nil {
		return errors.New("edit needs the terminal (not a pipe or redirection)")
	}
	fsys, rel, err := openPath(ctx, env, args[0])
	if err != nil {
		return err
	}
	text, err := fs.ReadFile(fsys, rel)
	note := ""
	if errors.Is(err, fs.ErrNotExist) {
		text, note = nil, "new file"
	} else if err != nil {
		return err
	}
	if _, _, werr := writable(ctx, env, args[0]); werr != nil {
		note = "read-only: " + werr.Error()
	}

	raw, release, err := env.TTY.Raw()
	if err != nil {
		return err
	}
	defer release()
	t := env.TTY
	cols, rows, early := terminalSize(ctx, env, raw)
	ed := edit.New(path.Base(rel), text, cols, rows)
	if note != "" {
		ed.Message(note)
	}
	prev := make([]string, rows)
	fmt.Fprint(t, "\x1b[0m\x1b[2J")
	draw := func() {
		screen, cr, cc := ed.Screen()
		var b strings.Builder
		b.WriteString("\x1b[?25l")
		for i, row := range screen {
			if i < len(prev) && prev[i] == row {
				continue
			}
			fmt.Fprintf(&b, "\x1b[%d;1H", i+1)
			if i == len(screen)-1 {
				b.WriteString("\x1b[7m" + row + "\x1b[0m")
			} else {
				b.WriteString(row)
			}
			if i < len(prev) {
				prev[i] = row
			}
		}
		fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[?25h", cr+1, cc+1)
		t.Write([]byte(b.String()))
	}
	defer fmt.Fprint(t, "\x1b[0m\x1b[2J\x1b[H")
	draw()

	var dec edit.Decoder
	var flush <-chan time.Time // a lone ESC waits briefly for the rest
	for {
		var keys []edit.Key
		if len(early) > 0 { // typed while the size query waited
			for _, b := range early {
				keys = append(keys, dec.Feed(b)...)
			}
			early = nil
		} else {
			select {
			case b := <-raw:
				keys = dec.Feed(b)
				if b == 0x1b {
					flush = time.After(50 * time.Millisecond)
				}
			case <-flush:
				flush = nil
				keys = dec.Flush()
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		for _, k := range keys {
			switch ed.Key(k) {
			case edit.Save:
				data := ed.Text()
				w, wrel, err := writable(ctx, env, args[0])
				if err == nil {
					err = w.WriteFile(wrel, data)
				}
				if err != nil {
					ed.Failed(err)
				} else {
					ed.Saved(len(data))
				}
			case edit.Quit:
				return nil
			}
		}
		if c, r := env.TTY.Size(); c > 0 && (c != cols || r != rows) { // an SSH window resized
			cols, rows = c, r
			ed.Resize(cols, rows)
			prev = make([]string, rows)
			fmt.Fprint(t, "\x1b[2J")
		}
		draw()
	}
}

// terminalSize is the TTY's known size, else what the terminal reports
// to a cursor-position query (serial terminals answer, the framebuffer
// console doesn't), else 80x24. Keys typed meanwhile are returned.
func terminalSize(ctx context.Context, env Env, raw <-chan byte) (cols, rows int, typed []byte) {
	if c, r := env.TTY.Size(); c > 0 {
		return c, r, nil
	}
	fmt.Fprint(env.TTY, "\x1b[999;999H\x1b[6n")
	var got []byte
	timeout := time.After(300 * time.Millisecond)
	for {
		select {
		case b := <-raw:
			got = append(got, b)
			if b != 'R' {
				continue
			}
			// the reply: ESC [ rows ; cols R
			i := strings.LastIndex(string(got), "\x1b[")
			if i < 0 {
				continue
			}
			r, c, ok := strings.Cut(string(got[i+2:len(got)-1]), ";")
			rn, err1 := strconv.Atoi(r)
			cn, err2 := strconv.Atoi(c)
			if !ok || err1 != nil || err2 != nil {
				continue // an R typed by hand
			}
			if rn < 3 || cn < 10 {
				rn, cn = 24, 80
			}
			return cn, rn, got[:i]
		case <-timeout:
			return 80, 24, got
		case <-ctx.Done():
			return 80, 24, got
		}
	}
}
