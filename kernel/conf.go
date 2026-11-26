package kernel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"strings"
	"sync"
	"time"

	"godeep/kernel/block"
	"godeep/kernel/cap"
	"godeep/kernel/dev"
	"godeep/kernel/proc"
	"godeep/kernel/vfs"
	"godeep/net/netstack"
)

// godeep.conf: settings on the board's data volume (the FAT32 partition
// labelled LogLabel, GODEEPDATA on the Mac), editable from macOS, read at
// boot even when the volume is read-only:
//
//	# comments
//	net = 192.168.1.50/24    static address (DHCP no longer changes it)
//	gateway = 192.168.1.1
//	dns = 192.168.1.1        repeatable
//	ntp = time.apple.com     time server (off: none)
//	run = lsdev usb          a shell command run at boot, repeatable
const confFile = "godeep/godeep.conf"

// ConfWait is how long confd looks for the data volume (USB sticks and the
// Mac's SSD take a few seconds to appear).
const ConfWait = 30 * time.Second

// Conf is a parsed godeep.conf.
type Conf struct {
	Net  netstack.Config // Addr invalid: not set
	NTP  string
	Run  []string
	Errs []string // lines that didn't parse
}

// ParseConf reads godeep.conf's "key = value" lines.
func ParseConf(r io.Reader) Conf {
	var c Conf
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		bad := func(why string) { c.Errs = append(c.Errs, fmt.Sprintf("line %d: %s", n, why)) }
		if !ok || v == "" {
			bad("not key = value")
			continue
		}
		switch strings.ToLower(k) {
		case "net":
			p, err := netip.ParsePrefix(v)
			if err != nil || !p.Addr().Is4() {
				bad("net: want an IPv4 address/prefix, like 192.168.1.50/24")
				continue
			}
			c.Net.Addr = p
		case "gateway":
			a, err := netip.ParseAddr(v)
			if err != nil {
				bad("gateway: " + err.Error())
				continue
			}
			c.Net.Gateway = a
		case "dns":
			a, err := netip.ParseAddr(v)
			if err != nil {
				bad("dns: " + err.Error())
				continue
			}
			c.Net.DNS = append(c.Net.DNS, a)
		case "ntp":
			c.NTP = v
		case "run":
			c.Run = append(c.Run, v)
		default:
			bad("unknown setting " + k)
		}
	}
	return c
}

// confd finds godeep.conf and applies it: the network (pinned), the time
// server, then the boot commands.
type confd struct {
	label string
	mgr   *dev.Manager
	fs    *vfs.NS
	log   io.Writer
	net   *cap.PendingNet
	clock *wallClock
	run   func(ctx context.Context, line string) // runs a shell command

	once sync.Once
}

// find returns the conf file's contents ("", false while there's none).
func (c *confd) find(ctx context.Context) (string, []byte, bool) {
	for name, d := range vfs.Disks(c.mgr) {
		parts, _, err := block.Partitions(ctx, d)
		if err != nil {
			continue
		}
		for _, p := range parts {
			if block.FAT32Label(ctx, p) != c.label {
				continue
			}
			vol := fmt.Sprintf("%sp%d", name, p.Index)
			v, err := c.fs.Open(ctx, vol)
			if err != nil {
				continue
			}
			if b, err := fs.ReadFile(v, confFile); err == nil {
				return vol, b, true
			}
		}
	}
	return "", nil, false
}

func (c *confd) start(ctx context.Context, tab *proc.Table) {
	tab.Spawn(ctx, 0, proc.Spec{Name: "confd", Main: func(ctx context.Context, p *proc.Proc) error {
		deadline := time.Now().Add(ConfWait)
		for {
			if vol, b, ok := c.find(ctx); ok {
				c.apply(ctx, vol, ParseConf(strings.NewReader(string(b))))
				return nil
			}
			if time.Now().After(deadline) {
				return nil // no conf: defaults
			}
			t := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil
			case <-t.C:
			}
		}
	}})
}

type pinner interface {
	Pin(cfg netstack.Config, source string) error
}

func (c *confd) apply(ctx context.Context, vol string, cf Conf) {
	n := 0
	if cf.Net.Addr.IsValid() {
		n++
	}
	if cf.NTP != "" {
		n++
	}
	fmt.Fprintf(c.log, "confd: %s:/%s: %d settings, %d commands\n", vol, confFile, n, len(cf.Run))
	for _, e := range cf.Errs {
		fmt.Fprintf(c.log, "confd: %s\n", e)
	}
	if cf.Net.Addr.IsValid() && c.net != nil {
		if st, err := c.net.Wait(ctx); err == nil {
			if p, ok := st.(pinner); ok {
				if err := p.Pin(cf.Net, "static (godeep.conf)"); err != nil {
					fmt.Fprintf(c.log, "confd: net: %v\n", err)
				} else {
					fmt.Fprintf(c.log, "confd: network set to %s\n", cf.Net.Addr)
				}
			}
		}
	}
	if cf.NTP != "" && c.clock != nil {
		c.clock.setServer(cf.NTP)
	}
	for _, line := range cf.Run {
		fmt.Fprintf(c.log, "confd: run %s\n", line)
		c.run(ctx, line)
	}
}
