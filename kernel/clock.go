package kernel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"godeep/kernel/cap"
	"godeep/kernel/dev"
)

// wallClock is the date the shell and TLS use. TamaGo's own clock counts
// from the timer every Go timer runs on, so it can't be moved after boot
// without firing every pending timeout; instead the wall clock is the
// system clock plus an offset, from the best source available: NTP, else
// a board RTC device named "rtc" (the Mac's SMC), else the system clock as
// it is (right on QEMU, whose board sets it from its RTC at boot).
type wallClock struct {
	mgr *dev.Manager
	net *cap.PendingNet

	kick chan string // a new time server (godeep.conf): sync now

	mu     sync.Mutex
	offset time.Duration
	source string // "" until NTP has answered
	synced time.Time
}

type sntpNet interface {
	SNTP(ctx context.Context, server string) (offset, rtt time.Duration, err error)
}

func (c *wallClock) Now() time.Time {
	now, _ := c.now()
	return now
}

func (c *wallClock) now() (time.Time, string) {
	c.mu.Lock()
	off, src := c.offset, c.source
	c.mu.Unlock()
	if src != "" {
		return time.Now().Add(off), src
	}
	if c.mgr != nil {
		for _, s := range c.mgr.List() {
			if clk, ok := s.Service.(dev.Clock); ok && s.Name == "rtc" && s.State == dev.Ready {
				if t := clk.Now(); !t.IsZero() {
					return t, "rtc"
				}
			}
		}
	}
	return time.Now(), "system clock"
}

// Source says where Now comes from.
func (c *wallClock) Source() string {
	_, src := c.now()
	return src
}

// Sync asks an NTP server and, if it answers, uses its time from now on.
// It returns the correction applied.
func (c *wallClock) Sync(ctx context.Context, server string) (time.Duration, error) {
	if c.net == nil {
		return 0, errors.New("ntp: no network stack on this board")
	}
	n, err := c.net.Wait(ctx)
	if err != nil {
		return 0, err
	}
	s, ok := n.(sntpNet)
	if !ok {
		return 0, errors.New("ntp: the network can't do UDP time queries")
	}
	off, rtt, err := s.SNTP(ctx, server)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.offset, c.source, c.synced = off, fmt.Sprintf("ntp %s (±%v)", server, (rtt/2).Round(time.Millisecond)), time.Now()
	c.mu.Unlock()
	return off, nil
}

// NTPEvery is how often ntpd resyncs.
const NTPEvery = time.Hour

// setServer switches ntpd to another server ("off": stop syncing).
func (c *wallClock) setServer(server string) {
	if c.kick == nil {
		return
	}
	select {
	case c.kick <- server:
	default:
	}
}

// ntpd syncs once the network is up, then every NTPEvery (every minute
// until the first answer).
func (c *wallClock) ntpd(ctx context.Context, server string, log io.Writer) {
	wait := time.Duration(0)
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case s := <-c.kick:
			t.Stop()
			if s == "off" {
				fmt.Fprintf(log, "ntpd: off (godeep.conf)\n")
				return
			}
			server = s
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		off, err := c.Sync(sctx, server)
		cancel()
		if err != nil {
			if ctx.Err() == nil && wait == 0 {
				fmt.Fprintf(log, "ntpd: %s: %v (retrying every minute)\n", server, err)
			}
			wait = time.Minute
			continue
		}
		fmt.Fprintf(log, "ntpd: %s: clock corrected by %v\n", server, off.Round(time.Millisecond))
		wait = NTPEvery
	}
}
