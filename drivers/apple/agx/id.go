// Package agx drives the M1 Pro's GPU: Apple's AGX (G13S, "apple,agx-t6000").
//
// Almost everything the GPU does is brokered by its firmware, which iBoot
// loads and which runs on an ASC coprocessor under RTKit. The kernel's
// side: power the GPU, start the coprocessor, agree with the firmware on
// the GPU's page tables (the UAT, through a "handoff" region iBoot sets
// up), hand the firmware a large init structure and ring-buffer channels,
// then submit work (vertex, fragment, compute) through those channels.
//
// Ported from Asahi Linux's drm/asahi (Rust, "GPL-2.0-only OR MIT": used
// here under MIT) and m1n1's proxyclient/m1n1/agx. See docs/AGX.md for
// the stages; this package implements stage 1 (bring-up): GPU identity,
// MMU fault decoding, UAT page tables, the handoff protocol, and the
// kernel's page-table slot.
package agx

import (
	"fmt"
	"math/bits"
	"strings"

	"godeep/kernel/mmio"
)

// Compatible is the GPU in the M1 Pro's device tree (Asahi: t6000.dtsi).
const Compatible = "apple,agx-t6000"

// HwConfig is what the driver must know about a chip before asking it
// anything (Asahi: hw/t600x.rs HWCONFIG_T6000, the parts stage 1 uses).
type HwConfig struct {
	ChipID      uint32
	Gen         Gen
	Variant     byte // 'S' for G13S
	Core        string
	OAS         uint // UAT output address bits (physical addresses)
	MaxClusters int
	MaxCores    int // per cluster
	BaseClockHz uint64
	// IdleOffStandbyTimerDefault is apple,idleoff-standby-timer's default.
	IdleOffStandbyTimerDefault uint32
	Init
}

// IOMap is a register range the firmware maps (Asahi: hw::IOMapping).
type IOMap struct {
	Base         uint64
	PerDie       bool
	Count        int
	Size, Stride uint64
	Writable     bool
}

// Init holds the chip constants InitData needs (Asahi: hw/t600x.rs; the
// unk_* names are the firmware fields they go to).
type Init struct {
	CoreID                     uint32 // GpuCore: G13S = 12
	NumDies                    int
	MaxFrags, MaxGPs           int
	DA87C                      int32
	DA8CC, DAE24               uint32
	DB454, DB534, DBAB8, DBABC uint32
	DB4E0                      uint64
	DBB30                      uint32
	Shared1Tab                 []int32
	Shared1A4                  uint32
	Shared2Tab                 []int32
	Shared2Unk508              uint32
	GlobalUnk54                uint16
	SramK                      float32
	CoefA, CoefB               []float32 // per cluster (one value each on t600x)
	GlobalTab                  []uint8
	FastSensorMask             [2]uint64
	FastSensorMaskAlt          [2]uint64
	FastDie0SensorPresent      uint32
	IOMaps                     []*IOMap // by firmware index; nil: unused
}

// T6000 is the M1 Pro.
var T6000 = HwConfig{ChipID: 0x6000, Gen: G13, Variant: 'S', Core: "G13S", OAS: 42,
	MaxClusters: 2, MaxCores: 8, BaseClockHz: 24_000_000,
	Init: Init{
		CoreID: 12, NumDies: 1, MaxFrags: 8, MaxGPs: 4,
		DA87C: 900, DA8CC: 11000, DAE24: 125,
		DB454: 1, DB4E0: 4, DB534: 1, DBAB8: 0x2084, DBABC: 0x80, DBB30: 0,
		Shared1Tab: []int32{0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff,
			0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff},
		Shared1A4:             0xffff,
		Shared2Tab:            []int32{-1, -1, -1, -1, 0x2aa, 0xaaa, -1, -1, 0, 0},
		Shared2Unk508:         0xcc00001,
		GlobalUnk54:           0xffff,
		SramK:                 1.02,
		CoefA:                 []float32{9.838, 9.819, 9.826, 9.799, 9.799, 9.826, 9.819, 9.838},
		CoefB:                 []float32{13, 13, 13, 13, 13, 13, 13, 13},
		GlobalTab:             []uint8{0, 1, 2, 1, 1, 90, 75, 1, 1, 1, 2, 90, 75, 1, 1, 1, 1, 90, 75, 1, 1},
		FastSensorMask:        [2]uint64{0x8080, 0},
		FastSensorMaskAlt:     [2]uint64{0x9090, 0},
		FastDie0SensorPresent: 0x03,
		IOMaps:                t600xIOMaps(4, false),
	}}

// t600xIOMaps is Asahi's iomaps(mcc_count, has_die1) for t600x.
func t600xIOMaps(mcc int, die1 bool) []*IOMap {
	m := func(base uint64, perDie bool, count int, size, stride uint64, w bool) *IOMap {
		return &IOMap{base, perDie, count, size, stride, w}
	}
	maps := []*IOMap{
		m(0x404d00000, false, 1, 0x1c000, 0, true), // Fender
		m(0x20e100000, false, 1, 0x4000, 0, false), // AICTimer
		m(0x28e104000, false, 1, 0x4000, 0, true),  // AICSWInt
		m(0x404000000, false, 1, 0x20000, 0, true), // RGX
		nil, // UVD
		nil, // unused
		nil, // DisplayUnderrunWA
		m(0x28e494000, true, 1, 0x4000, 0, false), // AnalogTempSensorControllerRegs
		nil, // PMPDoorbell
		m(0x404d80000, false, 1, 0x8000, 0, true),           // MetrologySensorRegs
		m(0x204d61000, false, 1, 0x1000, 0, true),           // GMGIFAFRegs
		m(0x200000000, true, mcc, 0xd8000, 0x1000000, true), // MCache registers
		nil, // AICBankedRegisters
		nil, // PMGRScratch
		m(0x2643c4000, false, 1, 0x1000, 0, true), // NIA special agent idle, die 0
		nil, // NIA, die 1
		nil, // CRE registers
		nil, // streaming codec registers
		m(0x28e3d0000, false, 1, 0x1000, 0, true),  // ?
		m(0x28e3c0000, false, 1, 0x2000, 0, false), // ?
	}
	if die1 {
		maps[15] = m(0x22643c4000, false, 1, 0x1000, 0, true)
	}
	return maps
}

// Firmware versions the reference driver has structure layouts for, on
// G13 ("apple,firmware-compat"; m1n1 maps 12.3.1 → 12.3, 13.5/13.6 → 13.5).
var supportedCompat = [][3]uint32{{12, 3, 0}, {13, 5, 0}}

// SupportedFirmware reports whether stage 2 (the init structure) will
// have a layout for this firmware.
func SupportedFirmware(compat [3]uint32) bool {
	for _, c := range supportedCompat {
		if c == compat {
			return true
		}
	}
	return false
}

// Gen is a GPU generation.
type Gen int

const (
	G13 Gen = 13 // M1 family
	G14 Gen = 14 // M2 family
)

// ID registers in the "sgx" window (Asahi: regs.rs).
const (
	ID_VERSION      = 0xd04000
	ID_UNK08        = 0xd04008
	ID_COUNTS_1     = 0xd04010
	ID_COUNTS_2     = 0xd04014
	ID_UNK18        = 0xd04018
	ID_CLUSTERS     = 0xd0401c
	CORE_MASK_0     = 0xd01500
	CORE_MASK_1     = 0xd01514
	CORE_MASKS_G14X = 0xe01500
)

// ID is the GPU's identity, read from its registers.
type ID struct {
	Gen         Gen
	G14X        bool
	Variant     byte   // P, G, S, C, D
	Rev         string // A0, A1, B0, B1, C0, C1
	Clusters    int
	Cores       int      // per cluster
	CoreMasks   []uint32 // active cores, one mask per cluster
	ActiveCores int
	GPs         int
	Raw         [6]uint32 // the six ID registers, for lsdev
}

func (id ID) String() string {
	g := fmt.Sprintf("G%d%c", id.Gen, id.Variant)
	if id.G14X {
		g = fmt.Sprintf("G14X%c", id.Variant)
	}
	var m []string
	for _, x := range id.CoreMasks {
		m = append(m, fmt.Sprintf("%#x", x))
	}
	return fmt.Sprintf("%s %s: %d clusters × %d cores, %d active (masks %s)",
		g, id.Rev, id.Clusters, id.Cores, id.ActiveCores, strings.Join(m, " "))
}

// ReadID reads and decodes the ID registers (Asahi: Resources::get_gpu_id).
// The GPU must be powered.
func ReadID(sgx mmio.Regs) (ID, error) {
	var id ID
	v := sgx.Read32(ID_VERSION)
	c1, c2 := sgx.Read32(ID_COUNTS_1), sgx.Read32(ID_COUNTS_2)
	cl := sgx.Read32(ID_CLUSTERS)
	id.Raw = [6]uint32{v, sgx.Read32(ID_UNK08), c1, c2, sgx.Read32(ID_UNK18), cl}
	if v == 0 || v == 0xffffffff {
		return id, fmt.Errorf("agx: ID register reads %#x (GPU not powered?)", v)
	}

	var masks []uint32
	var clusters uint32
	switch gen := v >> 24 & 0xff; gen {
	case 4, 5: // G13, G14G
		masks = []uint32{sgx.Read32(CORE_MASK_0), sgx.Read32(CORE_MASK_1)}
		clusters = cl >> 12 & 0xff
		id.Gen = G13
		if gen == 5 {
			id.Gen = G14
		}
	case 6: // G14X
		masks = []uint32{sgx.Read32(CORE_MASKS_G14X), sgx.Read32(CORE_MASKS_G14X + 4), sgx.Read32(CORE_MASKS_G14X + 8)}
		clusters = (c1 >> 8 & 0xff) * (c1 >> 16 & 0xf) // clusters per die × dies
		id.Gen, id.G14X = G14, true
	default:
		return id, fmt.Errorf("agx: unknown GPU generation %d (ID %#x)", gen, v)
	}

	cores := c1 & 0xff
	if cores == 0 || cores > 32 {
		return id, fmt.Errorf("agx: %d cores per cluster", cores)
	}
	if cores*clusters > uint32(len(masks))*32 {
		return id, fmt.Errorf("agx: too many cores (%d × %d > %d)", clusters, cores, len(masks)*32)
	}
	// The mask registers are one long bit string, cores bits per cluster.
	max := uint32(1<<cores - 1)
	for range clusters {
		m := masks[0] & max
		id.CoreMasks = append(id.CoreMasks, m)
		id.ActiveCores += bits.OnesCount32(m)
		for i := range masks {
			masks[i] = shr(masks[i], cores)
			if i < len(masks)-1 {
				masks[i] |= shl(masks[i+1], 32-cores)
			}
		}
	}
	for _, m := range masks {
		if m != 0 {
			return id, fmt.Errorf("agx: core mask bits left over (%#x)", masks)
		}
	}

	switch r := v >> 8 & 0xff; r {
	case 0x00, 0x01, 0x10, 0x11, 0x20, 0x21:
		id.Rev = fmt.Sprintf("%c%d", 'A'+r>>4, r&0xf)
	default:
		return id, fmt.Errorf("agx: unknown GPU revision %#x", r)
	}
	switch vr := v >> 16 & 0xff; vr {
	case 1:
		id.Variant = 'P'
	case 2:
		id.Variant = 'G'
	case 3:
		id.Variant = 'S'
	case 4:
		id.Variant = 'C'
		if clusters > 4 {
			id.Variant = 'D'
		}
	default:
		return id, fmt.Errorf("agx: unknown GPU variant %d", vr)
	}
	id.Clusters, id.Cores = int(clusters), int(cores)
	id.GPs = int(c2 >> 16 & 0xff)
	return id, nil
}

// shr and shl are shifts that give 0 for a shift of 32 (a 32-core cluster).
func shr(x, n uint32) uint32 { return uint32(uint64(x) >> n) }
func shl(x, n uint32) uint32 { return uint32(uint64(x) << n) }

// Check reports whether id is the GPU cfg describes.
func (cfg HwConfig) Check(id ID) error {
	if id.Gen != cfg.Gen || id.G14X || id.Variant != cfg.Variant {
		return fmt.Errorf("agx: found %s, expected %s", id, cfg.Core)
	}
	if id.Clusters > cfg.MaxClusters || id.Cores > cfg.MaxCores {
		return fmt.Errorf("agx: %s exceeds %s's %d × %d", id, cfg.Core, cfg.MaxClusters, cfg.MaxCores)
	}
	return nil
}
