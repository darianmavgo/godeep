//go:build tamago && arm64 && !applem1

#include "textflag.h"

// func psciCall(fn uint64) uint64
TEXT ·psciCall(SB),NOSPLIT,$0-16
	MOVD	fn+0(FP), R0
	MOVD	$0, R1
	MOVD	$0, R2
	MOVD	$0, R3
	HVC	$0
	MOVD	R0, ret+8(FP)
	RET

// func psciCallSMC(fn uint64) uint64
TEXT ·psciCallSMC(SB),NOSPLIT,$0-16
	MOVD	fn+0(FP), R0
	MOVD	$0, R1
	MOVD	$0, R2
	MOVD	$0, R3
	SMC	$0
	MOVD	R0, ret+8(FP)
	RET
