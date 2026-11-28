package agx

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
)

// RTKit endpoints and messages of the GPU firmware (Asahi: gpu.rs).
const (
	EPFirmware = 0x20
	EPDoorbell = 0x21

	MsgInit         = 0x81 << 48
	initDataMask    = 1<<44 - 1
	MsgTxDoorbell   = 0x83 << 48
	MsgFwCtl        = 0x84 << 48
	MsgRxDoorbell   = 0x42 << 48
	DoorbellKickFW  = 0x10
	DoorbellDevCtrl = 0x11
)

// initializeOp is the DeviceControlMsg discriminant of Initialize: the
// enum's last variant, which moved when 13.3 added Unk0d.
func initializeOp(compat [3]uint32) uint32 {
	if compat[0] > 13 || compat[0] == 13 && compat[1] >= 3 {
		return 0x1a
	}
	return 0x19
}

// fwLog keeps the firmware's log for lsdev and the shell.
type fwLog struct {
	mu      sync.Mutex
	lines   []string // last maxLog
	total   int
	dropped int // malformed entries
}

const maxLog = 64

var logLevels = []string{"debug", "info", "notice", "warn", "err", "crit"}

func (l *fwLog) add(sub int, s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	l.lines = append(l.lines, fmt.Sprintf("[%s] %s", logLevels[sub], s))
	if len(l.lines) > maxLog {
		l.lines = l.lines[len(l.lines)-maxLog:]
	}
}

// pollLog drains the firmware log channel (Asahi: FwLogChannel::poll).
// Each message names an entry in the payload buffer: per sub-channel
// (severity), 0x100 entries of {type 3, seq, timestamp, text[0xc8]}.
func pollLog(ch *Channels, l *fwLog) {
	payload := ch.FwLogPayload.Bytes()
	for sub := 0; sub < 6; sub++ {
		for {
			msg, ok := ch.FwLog.Get(sub)
			if !ok {
				break
			}
			typ := binary.LittleEndian.Uint32(msg)
			idx := binary.LittleEndian.Uint64(msg[8:])
			if typ != 2 || idx >= 0x100 {
				l.mu.Lock()
				l.dropped++
				l.mu.Unlock()
				continue
			}
			p := payload[(sub*0x100+int(idx))*FwLogPayloadSize:][:FwLogPayloadSize]
			text := p[0x10:]
			end := bytes.IndexByte(text, 0)
			if binary.LittleEndian.Uint32(p) != 3 || end < 0 {
				l.mu.Lock()
				l.dropped++
				l.mu.Unlock()
				continue
			}
			l.add(sub, string(text[:end]))
		}
	}
}

// drain empties a receive channel, counting its messages (events, stats
// and traces are for stage 3; nothing is submitted yet, so few arrive).
func drain(c *RxChannel) (n int) {
	for {
		if _, ok := c.Get(0); !ok {
			return n
		}
		n++
	}
}

// firmwareLoop handles the firmware's doorbells until ctx ends.
func firmwareLoop(ctx context.Context, recv <-chan uint64, ch *Channels, g *GPU) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-recv:
			if m != MsgRxDoorbell {
				g.mu.Lock()
				g.unknownMsgs++
				g.mu.Unlock()
				continue
			}
			pollLog(ch, &g.log)
			ev, st, kt := drain(ch.Event), drain(ch.Stats), drain(ch.KTrace)
			g.mu.Lock()
			g.doorbells++
			g.events += ev
			g.stats += st
			g.ktrace += kt
			g.mu.Unlock()
		}
	}
}
