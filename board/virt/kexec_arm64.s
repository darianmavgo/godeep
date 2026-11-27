//go:build tamago && arm64 && !applem1

#include "textflag.h"

// func kexec_jump(entry, fdt, ram0, ram1, dma0, dma1 uint64)
//
// Clean and invalidate the data cache over [ram0, ram1) and [dma0, dma1)
// (by VA, identity-mapped), so nothing dirty is written back over the next
// kernel later; then caches and MMU off, and jump to entry with x0 = fdt,
// x1-x3 = 0. Runs from this kernel's text, identity-mapped, so execution
// carries on at the same address once the MMU is off.
TEXT ·kexec_jump(SB),NOSPLIT|NOFRAME,$0-48
	MOVD	entry+0(FP), R9
	MOVD	fdt+8(FP), R10
	MOVD	ram0+16(FP), R0
	MOVD	ram1+24(FP), R1
	MOVD	dma0+32(FP), R2
	MOVD	dma1+40(FP), R3
	MSR	$0xf, DAIFSet

	BIC	$63, R0, R0
ram:
	WORD	$0xd50b7e20		// dc civac, x0
	ADD	$64, R0, R0
	CMP	R1, R0
	BLO	ram
	BIC	$63, R2, R2
dma:
	WORD	$0xd50b7e22		// dc civac, x2
	ADD	$64, R2, R2
	CMP	R3, R2
	BLO	dma
	DSB	$15

	MRS	SCTLR_EL1, R4
	MOVD	$((1<<0)|(1<<2)|(1<<12)), R5	// M, C, I
	BIC	R5, R4, R4
	MSR	R4, SCTLR_EL1
	ISB	$15
	WORD	$0xd508751f		// ic iallu
	TLBI	VMALLE1
	DSB	$15
	ISB	$15

	MOVD	R10, R0
	MOVD	$0, R1
	MOVD	$0, R2
	MOVD	$0, R3
	B	(R9)
