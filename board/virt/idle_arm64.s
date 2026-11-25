//go:build tamago && arm64

#include "textflag.h"

// func read_cntfrq() uint64
TEXT ·read_cntfrq(SB),NOSPLIT,$0-8
	MRS	CNTFRQ_EL0, R0
	MOVD	R0, ret+0(FP)
	RET

// func write_cntv_tval(ticks uint64)
TEXT ·write_cntv_tval(SB),NOSPLIT,$0-8
	MOVD	ticks+0(FP), R0
	MSR	R0, CNTV_TVAL_EL0
	ISB	$15
	RET

// func write_cntv_ctl(ctl uint64)
TEXT ·write_cntv_ctl(SB),NOSPLIT,$0-8
	MOVD	ctl+0(FP), R0
	MSR	R0, CNTV_CTL_EL0
	ISB	$15
	RET

// func wfi()
TEXT ·wfi(SB),NOSPLIT,$0
	DSB	$15
	WFI
	RET

// func read_midr() uint64
TEXT ·read_midr(SB),NOSPLIT,$0-8
	MRS	MIDR_EL1, R0
	MOVD	R0, ret+0(FP)
	RET
