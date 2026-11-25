//go:build tamago && arm64 && linkcpuinit && !applem1

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

// Link address of the Image (TEXT_START in the Makefile - 0x1000: the ELF
// header page that the Image header replaces). Must match the Makefile.
#define IMAGE_BASE 0x4000f000

// early identity map (see no_fdt below)
#define EARLY_CONSOLE 0x09000000		// PL011
#define NORMAL_BLOCK  0x705			// AF | inner shareable | MAIR idx 1 | block
#define DEVICE_BLOCK  0x0060000000000601	// UXN | PXN | AF | outer shareable | MAIR idx 0 | block

// cpuinit's absolute link address. Go assembles `MOVD $sym(SB)` as
// PC-relative (ADRP+ADD), which yields the *running* address; a data word
// holding the address keeps the link-time value wherever the image runs.
DATA ·cpuinitLink+0(SB)/8, $cpuinit(SB)
GLOBL ·cpuinitLink(SB), RODATA, $8

DATA ·bootFDT+0(SB)/8, $0
GLOBL ·bootFDT(SB), NOPTR, $8
DATA ·bootEL+0(SB)/8, $0
GLOBL ·bootEL(SB), NOPTR, $8

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
start:
	ADR	start, R1		// where cpuinit is running (PC-relative)
	MOVD	R0, R19			// device tree from the boot loader (or 0)

	// Self-relocation. Boot loaders don't all honour the Image's
	// text_offset: m1n1 puts the kernel at any 2 MB-aligned address in
	// its heap. TamaGo code is linked at a fixed address, so if we're
	// running elsewhere, copy the whole image (size from its header) to
	// IMAGE_BASE and continue there. Until then, only PC-relative code.
	MOVD	·cpuinitLink(SB), R2	// where it was linked
	CMP	R1, R2
	BEQ	relocated
	SUB	R2, R1, R3		// delta = runtime - link
	MOVD	$IMAGE_BASE, R4		// destination (link address of the image)
	ADD	R3, R4, R5		// source (where the loader put it)
	MOVD	16(R5), R6		// header image_size (includes bss)
	CMP	R4, R5
	BLO	copy_down		// source below destination: copy backwards
copy_up:				// forward copy (destination below source)
	CBZ	R6, copied_image
	MOVD	(R5), R7
	MOVD	R7, (R4)
	ADD	$8, R5, R5
	ADD	$8, R4, R4
	SUB	$8, R6, R6
	B	copy_up
copy_down:
	ADD	R6, R5, R5
	ADD	R6, R4, R4
copy_down_loop:
	CBZ	R6, copied_image
	SUB	$8, R5, R5
	SUB	$8, R4, R4
	MOVD	(R5), R7
	MOVD	R7, (R4)
	SUB	$8, R6, R6
	B	copy_down_loop
copied_image:
	DSB	$15
	WORD	$0xd508751f		// ic iallu
	DSB	$15
	ISB	$15
	MOVD	R19, R0
	B	(R2)			// restart at cpuinit's link address
relocated:
	MRS	CurrentEL, R20
	LSR	$2, R20, R20
	AND	$3, R20, R20		// entry exception level
	CMP	$2, R20
	BNE	at_el1

	// EL2 -> EL1 (docs/TargetMyMac.md §3.2). No stage-2 translation, no
	// traps, EL1 owns the timers and the GIC CPU interface.
	MOVD	$(1<<31), R0		// HCR_EL2.RW: EL1 is AArch64 (E2H, TGE, VM, traps 0)
	WORD	$0xd51c1100		// msr hcr_el2, x0
	ISB	$15
	MOVD	$0, R0
	WORD	$0xd51c2100		// msr vttbr_el2, x0: no stage-2 table
	// Apple cores force HCR_EL2.E2H = 1 (VHE), which changes the layout
	// of CPTR_EL2 and CNTHCTL_EL2: read E2H back and use the right one.
	WORD	$0xd53c1100		// mrs x0, hcr_el2
	TBNZ	$34, R0, vhe
	MOVD	$0x33ff, R0		// CPTR_EL2 (E2H=0): RES1 bits, TFP=0: no FP/SIMD traps
	B	cptr
vhe:
	MOVD	$(3<<20), R0		// CPTR_EL2 (E2H=1, CPACR layout): FPEN=0b11
cptr:
	WORD	$0xd51c1140		// msr cptr_el2, x0
	WORD	$0xd53ce100		// mrs x0, cnthctl_el2
	ORR	$0xc03, R0, R0		// EL1 timer/counter access: bits 0-1 (E2H=0) and 10-11 (E2H=1)
	WORD	$0xd51ce100		// msr cnthctl_el2, x0
	MOVD	$0, R0
	WORD	$0xd51ce060		// msr cntvoff_el2, x0
	WORD	$0xd51c1160		// msr hstr_el2, x0
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
	// SCTLR_EL1 to a known state: MMU, caches and alignment checks off,
	// little-endian, RES1 bits set (the firmware's value is unknown).
	MOVD	$0x30d00800, R0
	MSR	R0, SCTLR_EL1
	ISB	$15

	// Early identity map, so Go code (the page-table builder in Hwinit0)
	// runs on Normal memory: with the MMU off everything is Device memory
	// and Go's vector loads/stores take alignment faults. RAM is mapped with
	// 1 GB Normal blocks; the early console's GB as Device. Same TCR as the
	// final tables (48-bit VA, 4 KB granule), so RAM above 1 TB works.
	MOVD	runtime∕goos·RamStart(SB), R6
	MOVD	runtime∕goos·RamSize(SB), R7
	ADD	R6, R7, R7		// ramEnd
	ADD	$0x1000, R6, R8		// boot L0 table
	ADD	$0x2000, R6, R9		// boot L1 table
	MOVD	R8, R10
	MOVD	$1024, R11
zero:
	MOVD	ZR, (R10)
	ADD	$8, R10, R10
	SUB	$1, R11, R11
	CBNZ	R11, zero

	LSR	$39, R6, R10		// L0 index of RAM
	ORR	$3, R9, R11		// table descriptor
	MOVD	R11, (R8)(R10<<3)

	LSR	$30, R6, R10		// first GB of RAM
	SUB	$1, R7, R12
	LSR	$30, R12, R12		// last GB of RAM
	MOVD	$NORMAL_BLOCK, R13
gb:
	LSL	$30, R10, R14
	ORR	R13, R14, R14
	AND	$511, R10, R15
	MOVD	R14, (R9)(R15<<3)
	ADD	$1, R10, R10
	CMP	R12, R10
	BLS	gb

	MOVD	$EARLY_CONSOLE, R10	// its GB, as Device, if under the same L0 entry
	LSR	$39, R10, R14
	LSR	$39, R6, R15
	CMP	R14, R15
	BNE	no_console
	LSR	$30, R10, R10
	LSL	$30, R10, R14
	MOVD	$DEVICE_BLOCK, R13
	ORR	R13, R14, R14
	AND	$511, R10, R15
	MOVD	R14, (R9)(R15<<3)
no_console:

	MOVD	$0x44ff00, R0		// MAIR: Device-nGnRnE, Normal WB, Normal NC (kernel/pgtable.MAIR)
	MSR	R0, MAIR_EL1
	MRS	ID_AA64MMFR0_EL1, R0
	AND	$7, R0, R0
	LSL	$32, R0, R0		// IPS = PARange
	MOVD	$0x803510, R1		// EPD1, TG0 4K, SH0 inner, WB, T0SZ 16 (kernel/pgtable.TCR)
	ORR	R1, R0, R0
	MSR	R0, TCR_EL1
	MSR	R8, TTBR0_EL1
	DSB	$15
	TLBI	VMALLE1
	DSB	$15
	ISB	$15
	MRS	SCTLR_EL1, R0
	BIC	$(1<<19), R0, R0	// WXN off
	ORR	$(1<<12), R0, R0	// I-cache
	ORR	$(1<<2), R0, R0		// D-cache
	ORR	$(1<<0), R0, R0		// MMU on
	MSR	R0, SCTLR_EL1
	ISB	$15

	// stack at the top of RAM, then start the Go runtime
	MOVD	runtime∕goos·RamStart(SB), R1
	MOVD	R1, RSP
	MOVD	runtime∕goos·RamSize(SB), R1
	MOVD	runtime∕goos·RamStackOffset(SB), R2
	ADD	R1, RSP
	SUB	R2, RSP
	B	_rt0_tamago_start(SB)
