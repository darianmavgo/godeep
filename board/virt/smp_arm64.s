//go:build tamago && arm64 && !applem1

#include "textflag.h"

// func secondaryEntryAddr() uint64: where PSCI CPU_ON starts a core (the
// image runs identity-mapped at its link address, so PC-relative = physical).
TEXT ·secondaryEntryAddr(SB),NOSPLIT,$0-8
	MOVD	$·secondaryEntry(SB), R0
	MOVD	R0, ret+0(FP)
	RET

// func save_boot_regs(p *[5]uint64): TTBR0, TCR, MAIR, SCTLR, VBAR (EL1).
TEXT ·save_boot_regs(SB),NOSPLIT,$0-8
	MOVD	p+0(FP), R0
	MRS	TTBR0_EL1, R1
	MOVD	R1, 0(R0)
	MRS	TCR_EL1, R1
	MOVD	R1, 8(R0)
	MRS	MAIR_EL1, R1
	MOVD	R1, 16(R0)
	MRS	SCTLR_EL1, R1
	MOVD	R1, 24(R0)
	MRS	VBAR_EL1, R1
	MOVD	R1, 32(R0)
	RET

// func dcache_clean(addr, size uint64): clean to the point of coherency, so
// a core with its MMU (and caches) still off reads what we wrote.
TEXT ·dcache_clean(SB),NOSPLIT,$0-16
	MOVD	addr+0(FP), R0
	MOVD	size+8(FP), R1
	ADD	R0, R1, R1
	BIC	$63, R0, R0
loop:
	WORD	$0xd50b7a20		// dc cvac, x0
	ADD	$64, R0, R0
	CMP	R1, R0
	BLO	loop
	DSB	$15
	RET

// func sev()
TEXT ·sev(SB),NOSPLIT,$0
	DSB	$15
	SEV
	RET

// secondaryEntry: PSCI CPU_ON lands here at EL1, MMU off, DAIF masked,
// x0 = this core's slot index. IRQs stay masked for good.
TEXT ·secondaryEntry(SB),NOSPLIT|NOFRAME,$0
	MOVD	R0, R19

	MRS	CPACR_EL1, R1		// FP/SIMD: Go uses it everywhere
	ORR	$(3<<20), R1, R1
	MSR	R1, CPACR_EL1
	ISB	$15

	MOVD	$0, R1
	MSR	R1, CNTV_CTL_EL0	// virtual timer off until idle arms it

	MOVD	$·smpBoot(SB), R2	// the boot core's MMU and vectors
	MOVD	16(R2), R1
	MSR	R1, MAIR_EL1
	MOVD	8(R2), R1
	MSR	R1, TCR_EL1
	MOVD	0(R2), R1
	MSR	R1, TTBR0_EL1
	MOVD	32(R2), R1
	MSR	R1, VBAR_EL1
	DSB	$15
	TLBI	VMALLE1
	DSB	$15
	ISB	$15
	MOVD	24(R2), R1
	MSR	R1, SCTLR_EL1		// MMU and caches on
	ISB	$15

	MOVD	$1, R1			// GIC CPU interface: system registers,
	MSR	R1, ICC_SRE_EL1		// every priority, Group 0 (as the boot core)
	ISB	$15
	MOVD	$0xff, R1
	MSR	R1, ICC_PMR_EL1
	MOVD	$1, R1
	MSR	R1, ICC_IGRPEN0_EL1
	ISB	$15

	MOVD	$·smpAlive(SB), R2
	MOVW	$1, R1
	ADD	R19<<2, R2, R2
	STLRW	R1, (R2)

	MOVD	$·smpTask(SB), R3
	ADD	R19<<5, R3, R3
wait:
	LDAR	(R3), R4		// pc, written last by task()
	CBNZ	R4, run
	WFE
	B	wait
run:
	MOVD	8(R3), R5
	MOVD	R5, RSP
	MOVD	16(R3), g
	MOVD	$0, R29
	MOVD	$0, R30
	B	(R4)			// mstart: never returns
