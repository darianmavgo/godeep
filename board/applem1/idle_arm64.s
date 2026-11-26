//go:build tamago && arm64 && applem1

#include "textflag.h"

// func read_cntfrq() uint64
TEXT ·read_cntfrq(SB),NOSPLIT,$0-8
	MRS	CNTFRQ_EL0, R0
	MOVD	R0, ret+0(FP)
	RET

// func read_cntvct() uint64
TEXT ·read_cntvct(SB),NOSPLIT,$0-8
	ISB	$15
	MRS	CNTVCT_EL0, R0
	MOVD	R0, ret+0(FP)
	RET

// func read_cntkctl() uint64
TEXT ·read_cntkctl(SB),NOSPLIT,$0-8
	WORD	$0xd538e100		// mrs x0, cntkctl_el1
	MOVD	R0, ret+0(FP)
	RET

// func write_cntkctl(v uint64)
TEXT ·write_cntkctl(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	WORD	$0xd518e100		// msr cntkctl_el1, x0
	ISB	$15
	RET

// func wfe()
TEXT ·wfe(SB),NOSPLIT,$0
	WORD	$0xd503205f		// wfe
	RET
