//go:build tamago && arm64

#include "textflag.h"

// func read_cntpct() uint64
TEXT ·read_cntpct(SB),NOSPLIT,$0-8
	ISB	$15
	MRS	CNTPCT_EL0, R0
	MOVD	R0, ret+0(FP)
	RET

// func write_cntp_cval(v uint64)
TEXT ·write_cntp_cval(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	MSR	R0, CNTP_CVAL_EL0
	ISB	$15
	RET

// func write_cntp_ctl(v uint64)
TEXT ·write_cntp_ctl(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	MSR	R0, CNTP_CTL_EL0
	ISB	$15
	RET
