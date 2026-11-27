//go:build tamago && arm64 && !applem1

#include "textflag.h"

// func icc_iar0() uint64: acknowledge the highest-priority Group 0 interrupt.
TEXT ·icc_iar0(SB),NOSPLIT,$0-8
	MRS	ICC_IAR0_EL1, R0
	MOVD	R0, ret+0(FP)
	RET

// func icc_eoir0(id uint64)
TEXT ·icc_eoir0(SB),NOSPLIT,$0-8
	MOVD	id+0(FP), R0
	MSR	R0, ICC_EOIR0_EL1
	ISB	$15
	RET

// func read_mpidr() uint64
TEXT ·read_mpidr(SB),NOSPLIT,$0-8
	MRS	MPIDR_EL1, R0
	MOVD	R0, ret+0(FP)
	RET

// func icc_sgi0r(v uint64): generate a Group 0 SGI (ICC_SGI0R_EL1).
TEXT ·icc_sgi0r(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	DSB	$15			// the store the target waits on is visible first
	MSR	R0, ICC_SGI0R_EL1
	ISB	$15
	RET

// func daif_mask_irq(), daif_unmask_irq(): PSTATE.I on this core.
TEXT ·daif_mask_irq(SB),NOSPLIT,$0
	MSR	$2, DAIFSet
	ISB	$15
	RET

TEXT ·daif_unmask_irq(SB),NOSPLIT,$0
	MSR	$2, DAIFClr
	ISB	$15
	RET
