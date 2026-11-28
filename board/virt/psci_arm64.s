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

// func psciCall4(fn, a1, a2, a3 uint64) uint64 (HVC conduit; SMP needs an
// EL1 boot, where QEMU's PSCI is on HVC)
TEXT ·psciCall4(SB),NOSPLIT,$0-40
	MOVD	fn+0(FP), R0
	MOVD	a1+8(FP), R1
	MOVD	a2+16(FP), R2
	MOVD	a3+24(FP), R3
	HVC	$0
	MOVD	R0, ret+32(FP)
	RET

// func psciCall4SMC(fn, a1, a2, a3 uint64) uint64 (SMC conduit: Parallels
// Desktop, whose ACPI FADT says PSCI over SMC)
TEXT ·psciCall4SMC(SB),NOSPLIT,$0-40
	MOVD	fn+0(FP), R0
	MOVD	a1+8(FP), R1
	MOVD	a2+16(FP), R2
	MOVD	a3+24(FP), R3
	SMC	$0
	MOVD	R0, ret+32(FP)
	RET
