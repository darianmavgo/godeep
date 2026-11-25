//go:build tamago && arm64 && linkhwinit0 && applem1

#include "textflag.h"

// func read64(addr uint64) uint64
TEXT ·read64(SB),NOSPLIT,$0-16
	MOVD	addr+0(FP), R0
	MOVD	(R0), R1
	MOVD	R1, ret+8(FP)
	RET

// func write64(addr uint64, v uint64)
TEXT ·write64(SB),NOSPLIT,$0-16
	MOVD	addr+0(FP), R0
	MOVD	v+8(FP), R1
	MOVD	R1, (R0)
	RET

// func fp_enable(): CPACR_EL1.FPEN = 0b11 (no FP/SIMD traps at EL1/EL0).
// Go code uses SIMD registers (memclr, copy), so this must come first.
TEXT ·fp_enable(SB),NOSPLIT,$0
	MRS	CPACR_EL1, R0
	ORR	$(3<<20), R0, R0
	MSR	R0, CPACR_EL1
	ISB	$15
	RET

// func mmu_switch(ttbr0 uint64): replace the early identity map (cpuinit)
// with the final tables. Both map the running code identically, so the
// switch is a TTBR swap plus TLB invalidation; MAIR and TCR are unchanged.
TEXT ·mmu_switch(SB),NOSPLIT,$0-8
	MOVD	ttbr0+0(FP), R0
	DSB	$15			// table writes complete
	MSR	R0, TTBR0_EL1
	ISB	$15
	TLBI	VMALLE1
	DSB	$15
	ISB	$15
	RET
