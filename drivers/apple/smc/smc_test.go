package smc

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"godeep/drivers/apple/asc"
	"godeep/drivers/apple/asc/asctest"
	"godeep/kernel/mmio"
	"godeep/kernel/rtkit"
)

const sramBase = 0x2_91e0_0000

func TestSMC(t *testing.T) {
	sram := mmio.NewFake()
	keys := map[uint32][]byte{Key("BUIC"): {87}, Key("CHCE"): {1}, Key("MSLD"): {0},
		Key("CLKM"): {1, 2, 3, 4, 5, 6}, Key("BLOB"): []byte("0123456789")}
	var wmu sync.Mutex
	wrote := map[uint32]uint32{}
	written := func(k uint32) uint32 { wmu.Lock(); defer wmu.Unlock(); return wrote[k] }
	var model *asctest.Model
	model = asctest.NewModel(func(m rtkit.Message) []rtkit.Message {
		typ := m.Data >> 52 & 0xff
		if m.EP == 0 {
			switch typ {
			case 6: // SetIOP INIT (wake): start over with HELLO
				return []rtkit.Message{{EP: 0, Data: 1<<52 | 12<<16 | 12}}
			case 2:
				return []rtkit.Message{{EP: 0, Data: 8<<52 | 1<<51 | 1<<32 | 1}} // EP 0x20
			case 8:
				return []rtkit.Message{{EP: 0, Data: 7<<52 | rtkit.PwrOn}}
			case 0xb:
				return []rtkit.Message{{EP: 0, Data: m.Data}}
			}
			return nil
		}
		v := m.Data
		id := v >> 12 & 0xf
		key := uint32(v >> 32)
		switch v & 0xff {
		case msgInit:
			return []rtkit.Message{{EP: Endpoint, Data: sramBase + 0x4000}}
		case msgReadKey:
			val, ok := keys[key]
			if !ok {
				return []rtkit.Message{{EP: Endpoint, Data: id<<12 | 0x84}} // key not found
			}
			r := id<<12 | uint64(len(val))<<16
			if len(val) <= 4 {
				var w [4]byte
				copy(w[:], val)
				r |= uint64(binary.LittleEndian.Uint32(w[:])) << 32
			} else {
				for i := 0; i < len(val); i += 4 {
					var w [4]byte
					copy(w[:], val[i:])
					sram.Set(0x4000+uint64(i), binary.LittleEndian.Uint32(w[:]))
				}
			}
			return []rtkit.Message{{EP: Endpoint, Data: r}}
		case msgWriteKey:
			wmu.Lock()
			wrote[key] = sram.Get(0x4000)
			wmu.Unlock()
			return []rtkit.Message{{EP: Endpoint, Data: id << 12}}
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mb := asc.New(model.F)
	go mb.Run(ctx, nil)
	rk := rtkit.New("smc", mb, nil, nil)
	go rk.Run(ctx)
	s, err := Start(ctx, rk, sram, sramBase, 0x100000)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := s.Read(ctx, "BUIC", 1); err != nil || b[0] != 87 {
		t.Fatalf("BUIC %v %v", b, err)
	}
	if b, err := s.Read(ctx, "BLOB", 10); err != nil || string(b) != "0123456789" {
		t.Fatalf("BLOB %q %v", b, err)
	}
	if c, err := s.Clock(ctx); err != nil || c != 0x060504030201 {
		t.Fatalf("CLKM %#x %v", c, err)
	}
	if _, err := s.Read(ctx, "NOPE", 1); err == nil {
		t.Fatal("missing key read")
	}
	if d := s.Devices(); len(d) < 2 || d[1] != "battery 87%, charger connected, lid open" {
		t.Fatalf("Devices %q", d)
	}
	go s.PowerOff(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for written(Key("MBSE")) != Key("off1") {
		if time.Now().After(deadline) {
			t.Fatalf("MBSE = %#x", written(Key("MBSE")))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRTCArithmetic(t *testing.T) {
	// 2026-11-26 12:00:00 UTC in 1/32768 s, split between counter and offset
	want := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	total := uint64(want.Unix()) << 15
	ctr := uint64(123456789)
	off := (total - ctr) & (1<<48 - 1)
	sram := mmio.NewFake()
	keys := map[uint32][]byte{}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], ctr)
	keys[Key("CLKM")] = b[:6]
	model := asctest.NewModel(func(m rtkit.Message) []rtkit.Message {
		if m.EP == 0 {
			switch m.Data >> 52 & 0xff {
			case 2:
				return []rtkit.Message{{EP: 0, Data: 8<<52 | 1<<51 | 1<<32 | 1}}
			case 8:
				return []rtkit.Message{{EP: 0, Data: 7<<52 | rtkit.PwrOn}}
			case 0xb:
				return []rtkit.Message{{EP: 0, Data: m.Data}}
			}
			return nil
		}
		id := m.Data >> 12 & 0xf
		if m.Data&0xff == msgInit {
			return []rtkit.Message{{EP: Endpoint, Data: sramBase}}
		}
		v := keys[uint32(m.Data>>32)]
		for i := 0; i < len(v); i += 4 {
			var w [4]byte
			copy(w[:], v[i:])
			sram.Set(uint64(i), binary.LittleEndian.Uint32(w[:]))
		}
		return []rtkit.Message{{EP: Endpoint, Data: id<<12 | uint64(len(v))<<16}}
	})
	model.Post(rtkit.Message{EP: 0, Data: 1<<52 | 12<<16 | 12})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mb := asc.New(model.F)
	go mb.Run(ctx, nil)
	rk := rtkit.New("smc", mb, nil, nil)
	go rk.Run(ctx)
	s, err := Start(ctx, rk, sram, sramBase, 0x100000)
	if err != nil {
		t.Fatal(err)
	}
	if got := (&RTC{smc: s, offset: off}).Now(); !got.Equal(want) {
		t.Fatalf("Now = %v, want %v", got, want)
	}
}
