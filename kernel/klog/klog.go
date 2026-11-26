// Package klog is the kernel log: what the kernel and its services report
// (boot, network, logins), kept in a ring buffer so it can be read later
// (dmesg, or ssh godeep dmesg) and followed (dmesg -f), and mirrored to the
// console. Each line is stamped with the time since boot.
//
// A Log is passed to whoever logs (sh.Env.Log, sshd's Config.Log), not
// reached through a global.
package klog

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// DefaultSize is the ring buffer's size in bytes.
const DefaultSize = 64 << 10

// Log is a kernel log. Safe for concurrent use.
type Log struct {
	start   time.Time
	size    int
	console io.Writer // nil: not mirrored

	mu      sync.Mutex
	buf     []byte // complete lines, oldest first
	partial []byte // a line not ended yet
	dropped int    // bytes dropped from the front
	subs    map[chan []byte]struct{}
}

// New returns a log that mirrors to console (may be nil).
func New(console io.Writer, size int) *Log {
	if size <= 0 {
		size = DefaultSize
	}
	return &Log{start: time.Now(), size: size, console: console, subs: map[chan []byte]struct{}{}}
}

// Write logs p; complete lines are stamped, buffered, mirrored and sent to
// followers.
func (l *Log) Write(p []byte) (int, error) { return l.write(p, true) }

// Record logs p without mirroring it to the console (for text the console
// already shows, like the boot banner).
func (l *Log) Record(p []byte) { l.write(p, false) }

func (l *Log) write(p []byte, mirror bool) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		line := fmt.Appendf(nil, "[%10.3f] %s", time.Since(l.start).Seconds(), l.partial[:i+1])
		l.partial = l.partial[i+1:]
		l.buf = append(l.buf, line...)
		if over := len(l.buf) - l.size; over > 0 {
			// drop whole lines from the front
			cut := bytes.IndexByte(l.buf[over:], '\n') + over + 1
			l.dropped += cut
			l.buf = append(l.buf[:0:0], l.buf[cut:]...)
		}
		if mirror && l.console != nil {
			l.console.Write(line)
		}
		for c := range l.subs {
			select {
			case c <- line:
			default: // a slow follower misses lines, never blocks the kernel
			}
		}
	}
	return len(p), nil
}

// Printf logs a formatted line.
func (l *Log) Printf(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	if len(s) == 0 || s[len(s)-1] != '\n' {
		s += "\n"
	}
	l.Write([]byte(s))
}

// Bytes returns the buffered lines.
func (l *Log) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.buf...)
}

// Dropped is how many bytes have scrolled out of the buffer.
func (l *Log) Dropped() int { l.mu.Lock(); defer l.mu.Unlock(); return l.dropped }

// Follow returns new lines as they're logged, until ctx is done.
func (l *Log) Follow(ctx context.Context) <-chan []byte {
	c := make(chan []byte, 64)
	l.mu.Lock()
	l.subs[c] = struct{}{}
	l.mu.Unlock()
	context.AfterFunc(ctx, func() {
		l.mu.Lock()
		delete(l.subs, c)
		l.mu.Unlock()
		close(c)
	})
	return c
}
