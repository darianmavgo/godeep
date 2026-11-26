//go:build tamago && arm64 && applem1

#include "textflag.h"

// earlyFault is the target of every entry in the early exception vector
// table (installed by cpuinit, replaced by TamaGo's when CPU.Init runs). It
// saves the fault registers, switches to a small private stack (the faulting
// one may be the problem) and paints the fault screen (early.go). A fault
// while doing so just stops.
TEXT ·earlyFault(SB),NOSPLIT|NOFRAME,$0
	MRS	ESR_EL1, R0
	MOVD	R0, ·faultESR(SB)
	MRS	ELR_EL1, R0
	MOVD	R0, ·faultELR(SB)
	MRS	FAR_EL1, R0
	MOVD	R0, ·faultFAR(SB)
	MOVD	·faultDepth(SB), R0
	CBNZ	R0, hang
	MOVD	$1, R0
	MOVD	R0, ·faultDepth(SB)
	MOVD	runtime∕goos·RamStart(SB), R0
	ADD	$0x4000, R0, R0		// FAULT_STACK (early.go): 0x3000-0x4000 is free
	MOVD	R0, RSP
	BL	·earlyFaultGo(SB)
hang:
	WFE
	B	hang

// func read_esr() uint64
TEXT ·read_esr(SB),NOSPLIT,$0-8
	MRS	ESR_EL1, R0
	MOVD	R0, ret+0(FP)
	RET

// func read_far() uint64
TEXT ·read_far(SB),NOSPLIT,$0-8
	MRS	FAR_EL1, R0
	MOVD	R0, ret+0(FP)
	RET

// func tables_sync(): make page-table writes visible to the table walker.
TEXT ·tables_sync(SB),NOSPLIT,$0
	DSB	$15
	ISB	$15
	RET
