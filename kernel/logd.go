package kernel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"godeep/kernel/block"
	"godeep/kernel/dev"
	"godeep/kernel/klog"
	"godeep/kernel/proc"
	"godeep/kernel/vfs"
)

// logd saves the kernel log to a FAT32 partition with a given label (the
// Mac's GODEEPDATA), so a boot can be read afterwards from macOS instead of
// photographed: godeep/log.txt is this boot (rewritten every LogEvery while
// it grows, and on halt), godeep/log.prev.txt the one before.
const (
	LogEvery   = 5 * time.Second
	logDir     = "godeep"
	logFile    = "godeep/log.txt"
	logPrev    = "godeep/log.prev.txt"
	logTimeout = 3 * time.Second
)

type logSaver struct {
	label string
	mgr   *dev.Manager
	fs    *vfs.NS
	log   *klog.Log

	mu      sync.Mutex
	vol     string // the partition found ("nvme0p2")
	rotated bool
	saved   int // log bytes (ever) at the last write
}

func startLogd(ctx context.Context, tab *proc.Table, s *logSaver) {
	tab.Spawn(ctx, 0, proc.Spec{Name: "logd", Main: func(ctx context.Context, p *proc.Proc) error {
		t := time.NewTicker(LogEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				s.save(ctx)
			}
		}
	}})
}

// find returns the volume to write to: a writable FAT32 partition labelled
// s.label ("" if there's none yet).
func (s *logSaver) find(ctx context.Context) string {
	for name, d := range vfs.Disks(s.mgr) {
		parts, _, err := block.Partitions(ctx, d)
		if err != nil {
			continue
		}
		for _, p := range parts {
			if block.Writable(p) && block.FAT32Label(ctx, p) == s.label {
				return fmt.Sprintf("%sp%d", name, p.Index)
			}
		}
	}
	return ""
}

// save writes the log if it changed. Errors are reported once in the log
// itself, which then makes the next save retry.
func (s *logSaver) save(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.log.Bytes()
	total := len(b) + s.log.Dropped() // grows even once the ring is full
	if total == s.saved && s.vol != "" {
		return nil
	}
	if s.vol == "" {
		if s.vol = s.find(ctx); s.vol == "" {
			return errors.New("no writable partition labelled " + s.label)
		}
		s.log.Printf("logd: saving the kernel log to %s:/%s", s.vol, logFile)
		b = s.log.Bytes()
		total = len(b) + s.log.Dropped()
	}
	v, err := s.fs.Open(ctx, s.vol)
	if err != nil {
		s.vol = ""
		return err
	}
	w, ok := v.(vfs.Writable)
	if !ok || !vfs.IsWritable(v) {
		s.vol = ""
		return fmt.Errorf("%s is not writable", s.vol)
	}
	if !s.rotated {
		w.Mkdir(logDir) // exists after the first boot
		w.Remove(logPrev)
		w.Rename(logFile, logPrev)
		s.rotated = true
	}
	if err := w.WriteFile(logFile, b); err != nil {
		s.vol = "" // look again next time (the disk may have gone)
		return err
	}
	s.saved = total
	return nil
}

// flush is the last save before a shutdown stops the disk's driver.
func (s *logSaver) flush() {
	ctx, cancel := context.WithTimeout(context.Background(), logTimeout)
	defer cancel()
	if err := s.save(ctx); err != nil && s.vol != "" {
		s.log.Printf("logd: last save failed: %v", err)
	}
}
