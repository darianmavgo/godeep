// godeep patch: see exception.go FaultPanics and GODEEP_PATCHES.md.

//go:build godeepruntime

#include "textflag.h"

// faultPanic hands a synchronous fault from Go code to the runtime
// (runtime·tamagoFault, godeep's runtime patch), which panics in the
// faulting goroutine. It returns (to the fatal path) when FaultPanics is
// off, there's no g, or the exception isn't one a goroutine causes.
// R4 holds the interrupted code's LR; uses R0-R3.
TEXT ·faultPanic(SB),NOSPLIT|NOFRAME,$0
	MOVBU	·FaultPanics(SB), R0
	CBZ	R0, fatal
	CBZ	g, fatal
	MRS	ESR_EL1, R2
	LSR	$26, R2, R0
	AND	$0x3f, R0, R0
	CBZ	R0, ok			// undefined instruction
	CMP	$0x20, R0		// instruction aborts (0x20, 0x21), PC alignment (0x22)
	BLO	fatal
	CMP	$0x22, R0
	BLS	ok
	CMP	$0x24, R0		// data aborts (0x24, 0x25), SP alignment (0x26)
	BLO	fatal
	CMP	$0x26, R0
	BHI	fatal
ok:
	MRS	FAR_EL1, R0
	MRS	ELR_EL1, R1
	MRS	SPSR_EL1, R3
	MOVD	R4, R30
	B	runtime·tamagoFault(SB)
fatal:
	RET
