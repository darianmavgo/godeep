package agx

import (
	"fmt"

	"godeep/kernel/mmio"
)

// FAULT_INFO is the UAT fault status register on G13 (Asahi: regs.rs).
const FAULT_INFO = 0x17030

// Fault is a GPU MMU fault.
type Fault struct {
	Addr     uint64 // GPU virtual address
	Sideband uint8
	VMSlot   uint32 // UAT context (0: the kernel's)
	Unit     string // the GPU block that faulted
	Level    uint8
	Read     bool
	Reason   string
}

func (f Fault) String() string {
	rw := "write"
	if f.Read {
		rw = "read"
	}
	return fmt.Sprintf("%s fault at %#x (%s, unit %s, slot %d, level %d)", f.Reason, f.Addr, rw, f.Unit, f.VMSlot, f.Level)
}

// ReadFault returns the pending MMU fault on a G13 GPU, if any.
func ReadFault(sgx mmio.Regs64) (Fault, bool) { return DecodeFault(sgx.Read64(FAULT_INFO)) }

// DecodeFault decodes a G13 FAULT_INFO value (Asahi: get_fault_info).
func DecodeFault(info uint64) (Fault, bool) {
	if info&1 == 0 {
		return Fault{}, false
	}
	reasons := []string{"unmapped", "access flag", "write-only", "read-only", "no access"}
	reason := fmt.Sprintf("reason %d", info>>1&7)
	if r := info >> 1 & 7; int(r) < len(reasons) {
		reason = reasons[r]
	}
	return Fault{
		Addr:     info >> 30 << 6,
		Sideband: uint8(info >> 23 & 0x7f),
		VMSlot:   uint32(info >> 17 & 0x3f),
		Unit:     unitName(uint8(info >> 9 & 0xff)),
		Level:    uint8(info >> 7 & 3),
		Read:     info&(1<<4) != 0,
		Reason:   reason,
	}, true
}

// unitName names the GPU block behind a G13 fault unit code.
func unitName(c uint8) string {
	switch {
	case c <= 0x9f:
		names := []string{"DCMP", "UL1C", "CMP", "GSL1", "IAP", "VCE", "TE", "RAS",
			"VDM", "PPP", "IPF", "IPF_CPF", "VF", "VF_CPF", "ZLS"}
		if int(c&0xf) < len(names) {
			return fmt.Sprintf("%s%d", names[c&0xf], c>>4)
		}
	case c >= 0xa1 && c <= 0xae:
		return []string{"dPM", "dCDM_KS0", "dCDM_KS1", "dCDM_KS2", "dIPP", "dIPP_CS", "dVDM_CSD",
			"dVDM_SSD", "dVDM_ILF", "dVDM_ILD", "dRDE0", "dRDE1", "FC", "GSL2"}[c-0xa1]
	case c >= 0xb0 && c <= 0xb7:
		return fmt.Sprintf("GL2CC_META%d", c&0xf)
	case c == 0xb8:
		return "GL2CC_MB"
	case c >= 0xe0:
		return fmt.Sprintf("%s%d", []string{"gPM_SP", "gVDM_CSD_SP", "gVDM_SSD_SP", "gVDM_ILF_SP",
			"gVDM_TFP_SP", "gVDM_MMB_SP", "gCDM_CS_KS0_SP", "gCDM_CS_KS1_SP", "gCDM_CS_KS2_SP",
			"gCDM_KS0_SP", "gCDM_KS1_SP", "gCDM_KS2_SP", "gIPP_SP", "gIPP_CS_SP", "gRDE0_SP",
			"gRDE1_SP"}[c&0xf], c>>4&1)
	}
	return fmt.Sprintf("unknown(%#x)", c)
}
