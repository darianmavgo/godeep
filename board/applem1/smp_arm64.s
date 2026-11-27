//go:build tamago && arm64 && applem1

#include "textflag.h"

// func secondaryEntryAddr() uint64 (identity-mapped: PC-relative = physical)
TEXT ·secondaryEntryAddr(SB),NOSPLIT,$0-8
	MOVD	$·secondaryEntry(SB), R0
	MOVD	R0, ret+0(FP)
	RET

// func save_boot_regs(p *[6]uint64): TTBR0, TCR, MAIR, SCTLR, VBAR, CNTKCTL.
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
	WORD	$0xd538e101		// mrs x1, cntkctl_el1
	MOVD	R1, 40(R0)
	RET

// func dcache_clean(addr, size uint64): clean to the point of coherency.
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

// func dcache_civac(addr uint64): clean and invalidate one line.
TEXT ·dcache_civac(SB),NOSPLIT,$0-8
	MOVD	addr+0(FP), R0
	DSB	$15
	WORD	$0xd50b7e20		// dc civac, x0
	DSB	$15
	RET

// func sev()
TEXT ·sev(SB),NOSPLIT,$0
	DSB	$15
	SEV
	RET

// func read_mpidr() uint64
TEXT ·read_mpidr(SB),NOSPLIT,$0-8
	MRS	MPIDR_EL1, R0
	MOVD	R0, ret+0(FP)
	RET

// secondaryEntry: m1n1 releases a parked core here, at EL2 (E2H=1 on
// Apple cores), MMU off. Same EL2 -> EL1 drop as cpuinit (entry_arm64.s).
TEXT ·secondaryEntry(SB),NOSPLIT|NOFRAME,$0
	MRS	CurrentEL, R20
	LSR	$2, R20, R20
	AND	$3, R20, R20
	CMP	$2, R20
	BEQ	at_el2
	B	·secondaryEL1(SB)
at_el2:
	WORD	$0xd53c1101		// mrs x1, hcr_el2
	AND	$(1<<34), R1, R1	// keep E2H
	MOVD	$(1<<31), R0		// RW: EL1 is AArch64
	ORR	R1, R0, R0
	WORD	$0xd51c1100		// msr hcr_el2, x0
	ISB	$15
	MOVD	$0, R0
	WORD	$0xd51c2100		// msr vttbr_el2, x0
	WORD	$0xd53c1100		// mrs x0, hcr_el2
	TBNZ	$34, R0, vhe
	MOVD	$0x33ff, R0		// CPTR_EL2 (E2H=0)
	B	cptr
vhe:
	MOVD	$(3<<20), R0		// CPTR_EL2 (E2H=1): FPEN=0b11
cptr:
	WORD	$0xd51c1140		// msr cptr_el2, x0
	WORD	$0xd53ce100		// mrs x0, cnthctl_el2
	ORR	$0xc03, R0, R0		// EL1 timer/counter access
	WORD	$0xd51ce100		// msr cnthctl_el2, x0
	MOVD	$0, R0
	WORD	$0xd51ce060		// msr cntvoff_el2, x0
	WORD	$0xd51c1160		// msr hstr_el2, x0
	ISB	$15
	MOVD	$0x3c5, R0		// SPSR: EL1h, DAIF masked
	WORD	$0xd51c4000		// msr spsr_el2, x0
	MOVD	$·secondaryEL1(SB), R0
	WORD	$0xd51c4020		// msr elr_el2, x0
	ISB	$15
	ERET

TEXT ·secondaryEL1(SB),NOSPLIT|NOFRAME,$0
	// which slot is this core? (smpMPIDR, cleaned by the boot core)
	MRS	MPIDR_EL1, R0
	AND	$0xffffff, R0, R1
	LSR	$32, R0, R2
	AND	$0xff, R2, R2
	ORR	R2<<32, R1, R1		// affinity, as affinity() computes it
	MOVD	$·smpMPIDR(SB), R2
	MOVD	$0, R19
find:
	MOVD	(R2)(R19<<3), R3
	CMP	R1, R3
	BEQ	found
	ADD	$1, R19, R19
	CMP	$16, R19		// maxCPUs
	BLO	find
park:					// not ours to run: stay asleep
	WFE
	B	park
found:
	MRS	CPACR_EL1, R1		// FP/SIMD
	ORR	$(3<<20), R1, R1
	MSR	R1, CPACR_EL1
	ISB	$15

	MOVD	$·smpBoot(SB), R2
	MOVD	40(R2), R1
	WORD	$0xd518e101		// msr cntkctl_el1, x1: the event stream (idle)
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

	MOVD	$·smpAlive(SB), R2
	MOVW	$1, R1
	ADD	R19<<2, R2, R2
	STLRW	R1, (R2)

	MOVD	$·smpTask(SB), R3
	ADD	R19<<5, R3, R3
wait:
	LDAR	(R3), R4
	CBNZ	R4, run
	WFE
	B	wait
run:
	MOVD	8(R3), R5
	MOVD	R5, RSP
	MOVD	16(R3), g
	MOVD	$0, R29
	MOVD	$0, R30
	B	(R4)			// mstart
