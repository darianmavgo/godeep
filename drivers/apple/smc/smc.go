// Package smc talks to the M1 Pro's System Management Controller over
// RTKit (endpoint 0x20), after Linux's macsmc.c and m1n1's smc.c. The SMC
// is a key-value store: four-character keys ('BUIC' battery charge,
// 'MSLD' lid, 'CLKM' the RTC counter, 'MBSE' power actions). Values up to
// 4 bytes come back in the reply; longer ones are read from the shared
// buffer, which lives in the SMC's SRAM (Device memory, so word accesses).
//
// godeep uses it for power-off (MBSE = 'off1'), battery and lid status,
// and the RTC counter (wall time also needs an offset kept in the PMIC,
// see package spmi).
package smc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"godeep/drivers/apple/asc"
	"godeep/kernel/dev"
	"godeep/kernel/mmio"
	"godeep/kernel/rtkit"
)

// Compatible is the SMC in the M1 Pro's device tree.
const Compatible = "apple,t6000-smc"

const (
	Endpoint     = 0x20
	msgReadKey   = 0x10
	msgWriteKey  = 0x11
	msgKeyInfo   = 0x13
	msgInit      = 0x17
	msgNotify    = 0x18
	msgRWKey     = 0x20
	shmemSize    = 0x1000
	Timeout      = 500 * time.Millisecond
	BootTimeout  = 3 * time.Second
	maxValueSize = 255
)

// Key makes an SMC key from its four characters.
func Key(s string) uint32 {
	if len(s) != 4 {
		panic("smc: key " + s)
	}
	return uint32(s[0])<<24 | uint32(s[1])<<16 | uint32(s[2])<<8 | uint32(s[3])
}

// Config is what the board resolves from the device tree.
type Config struct {
	CPU, Mailbox uint64 // ASC CPU control and mailbox windows
	SRAM         uint64 // the "sram" window, where the shared buffer is
	SRAMSize     uint64
	MailboxIRQs  []int // the mailbox's interrupts (AIC ids), if the board has AIC
}

// SMC is the driver's service.
type SMC struct {
	mu     sync.Mutex // one command at a time
	send   func(context.Context, uint64) error
	recv   <-chan uint64
	sram   mmio.Regs
	shmem  uint64 // offset of the shared buffer in SRAM
	id     uint32
	notes  []uint32 // notifications seen (power button, lid, ...)
	nmu    sync.Mutex
	rtInfo func() rtkit.Info
	now    func(rtkit.Message) bool // asc.Mailbox.SendNow
}

// Driver returns the SMC driver; ready is called with the SMC once it
// answers (the board uses it for power-off).
func Driver(regs func(base uint64) mmio.Regs64, ready func(*SMC)) dev.Driver {
	return dev.Driver{Compatible: Compatible, Run: func(ctx context.Context, d *dev.Device) error {
		c, ok := d.Config.(Config)
		if !ok {
			if err, ok := d.Config.(error); ok {
				return err
			}
			return errors.New("smc: no configuration from the board")
		}
		mb := asc.New(regs(c.Mailbox))
		line := asc.RecvLine(d.Bridge, c.MailboxIRQs, "smc mailbox")
		d.Proc.Go(func(ctx context.Context) { mb.Run(ctx, line) })
		rk := rtkit.New("smc", mb, nil, nil) // the SMC's buffers live in its SRAM
		d.Proc.Go(func(ctx context.Context) {
			if err := rk.Run(ctx); err != nil && ctx.Err() == nil {
				d.Proc.Kill() // crashed: restart the driver
			}
		})
		// As m1n1's rtkit_boot: set the CPU's run bit whatever it reads
		// (a no-op if iBoot left it running), then INIT wakes the firmware
		// whether it was running, asleep or just started.
		cpu := regs(c.CPU)
		was := cpu.Read32(asc.CPU_CONTROL)
		asc.StartCPU(cpu)
		s, err := Start(ctx, rk, regs(c.SRAM), c.SRAM, c.SRAMSize)
		if err != nil {
			return fmt.Errorf("%w (CPU control was %#x; %s)", err, was, mb.Diag())
		}
		s.now = mb.SendNow
		d.Ready(s)
		if ready != nil {
			ready(s)
		}
		<-ctx.Done()
		return nil
	}}
}

// Start wakes the SMC firmware (IOP INIT, which works whether iBoot left
// it running or asleep, Linux's apple_rtkit_wake), starts its endpoint and
// sets up the shared buffer. The ASC's CPU must be running.
func Start(ctx context.Context, rk *rtkit.RTKit, sram mmio.Regs, sramBase, sramSize uint64) (*SMC, error) {
	bctx, cancel := context.WithTimeout(ctx, BootTimeout)
	defer cancel()
	if err := rk.Wake(bctx); err != nil {
		return nil, fmt.Errorf("smc: firmware: %w", err)
	}
	recv, send, err := rk.Endpoint(bctx, Endpoint)
	if err != nil {
		return nil, err
	}
	s := &SMC{send: send, recv: recv, sram: sram, rtInfo: rk.Info}
	if err := send(bctx, msgInit); err != nil {
		return nil, err
	}
	select { // the first answer is the shared buffer's address
	case v := <-recv:
		base := v // a physical address inside the SRAM window
		if base < sramBase || base+shmemSize > sramBase+sramSize {
			return nil, fmt.Errorf("smc: shared buffer at %#x is outside SRAM", base)
		}
		s.shmem = base - sramBase
	case <-bctx.Done():
		return nil, errors.New("smc: no shared buffer from the firmware")
	}
	return s, nil
}

// cmd sends one command and waits for its reply.
func (s *SMC) cmd(ctx context.Context, msg uint8, arg uint32, size, wsize int) (uint64, error) {
	s.id = (s.id + 1) & 0xf
	v := uint64(msg) | uint64(s.id)<<12 | uint64(size)<<16 | uint64(wsize)<<24 | uint64(arg)<<32
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	if err := s.send(ctx, v); err != nil {
		return 0, err
	}
	for {
		select {
		case r := <-s.recv:
			if r&0xff == msgNotify {
				s.nmu.Lock()
				if len(s.notes) < 64 {
					s.notes = append(s.notes, uint32(r>>32))
				}
				s.nmu.Unlock()
				continue
			}
			if uint32(r>>12&0xf) != s.id {
				continue // a late reply to an earlier, timed-out command
			}
			if res := r & 0xff; res != 0 {
				return r, fmt.Errorf("smc: command %#x key %s: result %#x", msg, keyName(arg), res)
			}
			return r, nil
		case <-ctx.Done():
			return 0, fmt.Errorf("smc: command %#x key %s: %w", msg, keyName(arg), ctx.Err())
		}
	}
}

func keyName(k uint32) string {
	return string([]byte{byte(k >> 24), byte(k >> 16), byte(k >> 8), byte(k)})
}

// Read returns a key's value (n bytes, little-endian on this SMC).
func (s *SMC) Read(ctx context.Context, key string, n int) ([]byte, error) {
	if n <= 0 || n > maxValueSize {
		return nil, fmt.Errorf("smc: read of %d bytes", n)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.cmd(ctx, msgReadKey, Key(key), n, 0)
	if err != nil {
		return nil, err
	}
	got := min(int(r>>16&0xffff), n)
	out := make([]byte, got)
	if got <= 4 {
		var w [4]byte
		binary.LittleEndian.PutUint32(w[:], uint32(r>>32))
		copy(out, w[:])
		return out, nil
	}
	for i := 0; i < got; i += 4 {
		var w [4]byte
		binary.LittleEndian.PutUint32(w[:], s.sram.Read32(s.shmem+uint64(i)))
		copy(out[i:], w[:])
	}
	return out, nil
}

// WriteU32 writes a 4-byte key (through the shared buffer).
func (s *SMC) WriteU32(ctx context.Context, key string, v uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sram.Write32(s.shmem, v)
	_, err := s.cmd(ctx, msgWriteKey, Key(key), 4, 0)
	return err
}

// PowerOff asks the SMC to cut power (Linux's macsmc-reboot: MBSE =
// 'off1'). It returns only if the SMC refused.
func (s *SMC) PowerOff(ctx context.Context) error {
	if err := s.WriteU32(ctx, "MBSE", Key("off1")); err != nil {
		return err
	}
	time.Sleep(2 * time.Second)
	return errors.New("smc: still running 2 s after power-off")
}

// PowerOffNow is PowerOff for when the scheduler can't be relied on (the
// runtime's exit path): it writes the value and posts the command straight
// to the mailbox, without waiting for the reply.
func (s *SMC) PowerOffNow() bool {
	if s.now == nil {
		return false
	}
	s.sram.Write32(s.shmem, Key("off1"))
	id := (s.id + 1) & 0xf
	v := uint64(msgWriteKey) | uint64(id)<<12 | 4<<16 | uint64(Key("MBSE"))<<32
	return s.now(rtkit.Message{EP: Endpoint, Data: v})
}

// Clock returns the SMC's RTC counter in seconds (CLKM: 48 bits, 1/32768
// s). Wall time = counter + the PMIC's offset.
func (s *SMC) Clock(ctx context.Context) (counter uint64, err error) {
	b, err := s.Read(ctx, "CLKM", 6)
	if err != nil {
		return 0, err
	}
	var w [8]byte
	copy(w[:], b)
	return binary.LittleEndian.Uint64(w[:]), nil
}

// Devices reports battery, charger and lid for lsdev.
func (s *SMC) Devices() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	in := s.rtInfo()
	out := []string{fmt.Sprintf("RTKit v%d, endpoints %x", in.Version, in.Endpoints)}
	line := ""
	if b, err := s.Read(ctx, "BUIC", 1); err == nil {
		line += fmt.Sprintf("battery %d%%", b[0])
	} else {
		line += fmt.Sprintf("battery: %v", err)
	}
	if b, err := s.Read(ctx, "CHCE", 1); err == nil {
		if b[0] != 0 {
			line += ", charger connected"
		} else {
			line += ", on battery"
		}
	}
	if b, err := s.Read(ctx, "MSLD", 1); err == nil {
		if b[0] != 0 {
			line += ", lid closed"
		} else {
			line += ", lid open"
		}
	}
	out = append(out, line)
	s.nmu.Lock()
	if len(s.notes) > 0 {
		out = append(out, fmt.Sprintf("notifications: %x", s.notes))
	}
	s.nmu.Unlock()
	return out
}

// RTCCompatible is the board's node for the clock below.
const RTCCompatible = "godeep,smc-rtc"

// RTCConfig says where the RTC's offset lives: PMIC NVMEM over SPMI.
type RTCConfig struct {
	SPMI string // the SPMI bus's device name
	SID  byte   // the PMIC's slave ID
	Addr uint16 // the 6-byte rtc-offset cell
}

// RTC is a dev.Clock: the SMC's counter (CLKM) plus the offset the PMIC
// keeps, as Linux's rtc-macsmc does. godeep's system clock still counts
// from boot (TamaGo derives wall time from the timer every Go timer uses,
// so moving it mid-boot would fire every pending timeout); `date` and TLS
// ask this clock instead.
type RTC struct {
	smc    *SMC
	offset uint64
}

// Now returns the time, or the zero time if the SMC doesn't answer.
func (r *RTC) Now() time.Time {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctr, err := r.smc.Clock(ctx)
	if err != nil {
		return time.Time{}
	}
	v := (ctr + r.offset) << 16 // sign-extend 48 bits, then 1/32768 s units
	ticks := int64(v) >> 16
	sec := ticks >> 15
	nsec := (ticks & (1<<15 - 1)) * 1e9 >> 15
	return time.Unix(sec, nsec)
}

// rtcReader is what the RTC needs from the SPMI bus.
type rtcReader interface {
	Read(sid byte, addr uint16, n int) ([]byte, error)
}

// RTCDriver publishes the RTC once the SMC and the SPMI bus are up.
func RTCDriver() dev.Driver {
	return dev.Driver{Compatible: RTCCompatible, Run: func(ctx context.Context, d *dev.Device) error {
		c, ok := d.Config.(RTCConfig)
		if !ok {
			return errors.New("rtc: no configuration from the board")
		}
		v, err := d.Lookup(ctx, "smc")
		if err != nil {
			return err
		}
		s, ok := v.(*SMC)
		if !ok {
			return errors.New("rtc: smc is not an SMC")
		}
		v, err = d.Lookup(ctx, c.SPMI)
		if err != nil {
			return err
		}
		bus, ok := v.(rtcReader)
		if !ok {
			return fmt.Errorf("rtc: %s is not an SPMI bus", c.SPMI)
		}
		b, err := bus.Read(c.SID, c.Addr, 6)
		if err != nil {
			return fmt.Errorf("rtc: offset: %w", err)
		}
		var w [8]byte
		copy(w[:], b)
		d.Ready(dev.Clock(&RTC{smc: s, offset: binary.LittleEndian.Uint64(w[:])}))
		<-ctx.Done()
		return nil
	}}
}
