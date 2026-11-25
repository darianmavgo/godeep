//go:build tamago && arm64 && linkcpuinit

#include "textflag.h"

// Boot entry for QEMU virt, shaped for Apple Silicon too.
//
// Replaces TamaGo's arm64 cpuinit (build tag linkcpuinit) to:
//   1. keep the device tree the boot loader passes in x0 (QEMU Image boot,
//      m1n1, U-Boot booti), copying it out of the way of the Go heap;
//   2. enter at EL2 (QEMU virtualization=on, m1n1) and drop to EL1, which
//      TamaGo's version can't do (it only handles EL1 and EL3).

// FDT copy area: the first 2MB of the DMA region (see mem.go).
#define FDT_COPY 0x120000000
#define FDT_MAX  0x200000

DATA ·bootFDT+0(SB)/8, $0
GLOBL ·bootFDT(SB), NOPTR, $8
DATA ·bootEL+0(SB)/8, $0
GLOBL ·bootEL(SB), NOPTR, $8

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
	MOVD	R0, R19			// device tree from the boot loader (or 0)
	MRS	CurrentEL, R20
	LSR	$2, R20, R20
	AND	$3, R20, R20		// entry exception level
	CMP	$2, R20
	BNE	at_el1

	// EL2 -> EL1. No stage-2 translation, no traps, EL1 owns the timers
	// and the GIC CPU interface.
	MOVD	$(1<<31), R0		// HCR_EL2.RW: EL1 is AArch64
	WORD	$0xd51c1100		// msr hcr_el2, x0
	WORD	$0xd53ce100		// mrs x0, cnthctl_el2
	ORR	$3, R0, R0		// EL1PCTEN | EL1PCEN
	WORD	$0xd51ce100		// msr cnthctl_el2, x0
	MOVD	$0, R0
	WORD	$0xd51ce060		// msr cntvoff_el2, x0
	WORD	$0xd51c1160		// msr hstr_el2, x0
	MOVD	$0x33ff, R0		// no FP/SIMD traps
	WORD	$0xd51c1140		// msr cptr_el2, x0
	MOVD	$9, R0			// ICC_SRE_EL2: SRE | Enable (EL1 may use ICC_*)
	WORD	$0xd51cc9a0		// msr icc_sre_el2, x0
	ISB	$15
	MOVD	$0x3c5, R0		// SPSR: EL1h, DAIF masked
	WORD	$0xd51c4000		// msr spsr_el2, x0
	MOVD	$·el1entry(SB), R0
	WORD	$0xd51c4020		// msr elr_el2, x0
	ISB	$15
	ERET

at_el1:
	B	·el1entry(SB)

TEXT ·el1entry(SB),NOSPLIT|NOFRAME,$0
	MOVD	$·bootEL(SB), R1
	MOVD	R20, (R1)

	// Copy a valid device tree (magic 0xd00dfeed, big endian) to FDT_COPY.
	CBZ	R19, no_fdt
	MOVWU	(R19), R1
	MOVW	$0xedfe0dd0, R2
	CMPW	R2, R1
	BNE	no_fdt
	MOVWU	4(R19), R1		// totalsize (big endian)
	REVW	R1, R1
	MOVD	$FDT_MAX, R2
	CMP	R2, R1
	BHI	no_fdt
	ADD	$7, R1, R1
	BIC	$7, R1, R1
	MOVD	R19, R3
	MOVD	$FDT_COPY, R4
copy:
	CBZ	R1, copied
	MOVD	(R3), R5
	MOVD	R5, (R4)
	ADD	$8, R3, R3
	ADD	$8, R4, R4
	SUB	$8, R1, R1
	B	copy
copied:
	MOVD	$FDT_COPY, R1
	MOVD	$·bootFDT(SB), R2
	MOVD	R1, (R2)
no_fdt:
	// What TamaGo's cpuinit_el1 does: MMU and alignment checks off, stack
	// at the top of RAM, then start the Go runtime.
	MRS	SCTLR_EL1, R0
	BIC	$(1<<1), R0, R0
	BIC	$(1<<0), R0, R0
	MSR	R0, SCTLR_EL1
	ISB	$15
	MOVD	runtime∕goos·RamStart(SB), R1
	MOVD	R1, RSP
	MOVD	runtime∕goos·RamSize(SB), R1
	MOVD	runtime∕goos·RamStackOffset(SB), R2
	ADD	R1, RSP
	SUB	R2, RSP
	B	_rt0_tamago_start(SB)
