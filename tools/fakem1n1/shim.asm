// fakem1n1 shim: the part of m1n1's hand-off that matters to godeep, for
// QEMU sbsa-ref. QEMU starts every core here at EL3 (-device
// loader,cpu-num=N). Core 0:
//   1. enables the PL011 and prints "m1n1-shim EL3"
//   2. drops to EL2 with the HCR_EL2 value from `hcr` (E2H=1 like Apple cores)
//   3. prints "EL2 E2H=<0|1>" (what the CPU kept), then jumps to the kernel
//      Image with the device tree in x0, x1-x3 = 0 (Linux arm64 boot protocol)
// Other cores (-smp N) drop to EL2 the same way, then wait on a spin table
// like m1n1 parks them for Linux: 8 bytes per core (Aff1*8 + Aff0) at `spin`, the
// device tree's cpu-release-addr; the kernel writes an entry point there and
// sends SEV. Like m1n1's smp_secondary_entry, the core then *calls* the entry
// (blr, x0-x3 = 0) on a stack of its own (spin + 4 KB + 512 B per core), at EL2.
// Two m1n1 states can be rehearsed (fakem1n1 -park, -secmmu):
//   park = 1: wait with WFI instead of WFE (m1n1's deep-WFI mode): SEV
//             doesn't wake the core, only an interrupt would
//   ptbase != 0: turn the EL2 MMU on first (identity map, E2H=1 format)
//             with the translation tables at ptbase, as m1n1 might leave it
//
// Assembled with Apple clang (`clang -c -x assembler -arch arm64 shim.asm`); the machine
// words are embedded in shim_code.go. Regenerate them with `go generate`
// (gen_shim.go) after changing this file. The seven parameter words at the end
// are patched by fakem1n1: keep them last and in this order.

	.text
	.globl	_start
	.p2align 2
_start:
	mrs	x13, mpidr_el1
	and	x12, x13, #0xff		// Aff0
	ubfx	x13, x13, #8, #8	// Aff1: sbsa-ref has 8 cores per cluster
	add	x12, x12, x13, lsl #3	// core number; 0 is the boot core
	ldr	x9, uart
	cbnz	x12, el3setup
	mov	w10, #0x301		// UARTCR: UARTEN | TXE | RXE
	str	w10, [x9, #0x30]
	adr	x11, msg_el3
	bl	puts
el3setup:

	msr	cptr_el3, xzr		// no FP/SIMD/SVE traps to EL3
	mov	x0, #0x5b1		// SCR_EL3: NS | RES1(4,5) | SMD | HCE | RW
	msr	scr_el3, x0
	mov	x0, #0x0830		// SCTLR_EL2: RES1 bits, MMU and caches off
	movk	x0, #0x30c5, lsl #16
	msr	sctlr_el2, x0
	ldr	x0, hcr
	msr	hcr_el2, x0
	msr	cptr_el2, xzr		// (E2H=1: FPEN=0 traps FP at EL0/1 until the kernel sets it)
	mov	x0, #0x3c9		// SPSR: EL2h, DAIF masked
	msr	spsr_el3, x0
	adr	x0, el2
	msr	elr_el3, x0
	isb
	eret

el2:
	cbnz	x12, secondary
	adr	x11, msg_el2
	bl	puts
	mrs	x0, hcr_el2
	ubfx	x0, x0, #34, #1		// E2H as the CPU kept it
	add	w10, w0, #'0'
	str	w10, [x9]
	mov	w10, #'\n'
	str	w10, [x9]

	ldr	x0, fdt
	mov	x1, xzr
	mov	x2, xzr
	mov	x3, xzr
	ldr	x4, kernel
	br	x4

secondary:
	ldr	x13, spin
	add	x14, x13, #0x1000	// this core's stack: spin + 4 KB + 512 B per core
	add	x15, x12, #1
	add	x14, x14, x15, lsl #9
	mov	sp, x14
	add	x13, x13, x12, lsl #3	// this core's cpu-release-addr

	ldr	x5, ptbase
	cbz	x5, park
	mov	x6, #0xff00		// MAIR: attr0 Device-nGnRnE, attr1 Normal WB
	msr	mair_el2, x6
	movz	x6, #0x3510		// TCR (EL1 layout, E2H=1): T0SZ=16, WB WA, inner shareable, 4K
	movk	x6, #0x0080, lsl #16	// EPD1: no TTBR1 walks
	movk	x6, #0x0005, lsl #32	// IPS: 48-bit
	msr	tcr_el2, x6
	msr	ttbr0_el2, x5
	isb
	tlbi	alle2
	dsb	sy
	isb
	movz	x6, #0x1835		// SCTLR_EL2: RES1 bits | I | C | M
	movk	x6, #0x30c5, lsl #16
	msr	sctlr_el2, x6
	isb

park:
	ldr	x6, parkmode
1:	ldr	x4, [x13]
	cbnz	x4, 2f
	cbnz	x6, 3f
	wfe
	b	1b
3:	wfi
	b	1b
2:	mov	x0, xzr
	mov	x1, xzr
	mov	x2, xzr
	mov	x3, xzr
	blr	x4			// m1n1 calls target(args...)
4:	wfe				// returned: nothing more to do
	b	4b

// puts: x11 = NUL-terminated string, x9 = UART; clobbers w10, x11
puts:
	ldrb	w10, [x11], #1
	cbz	w10, 1f
	str	w10, [x9]
	b	puts
1:	ret

msg_el3:
	.asciz	"m1n1-shim EL3\n"
msg_el2:
	.asciz	"m1n1-shim EL2 E2H="

	.p2align 3
uart:	.quad	0x60000000		// patched: PL011 base
hcr:	.quad	0			// patched: HCR_EL2 for the kernel
fdt:	.quad	0			// patched: device tree address
kernel:	.quad	0			// patched: kernel Image address
spin:	.quad	0			// patched: spin table (cpu-release-addr of core 0)
ptbase:	.quad	0			// patched: secondaries' EL2 translation tables (0: MMU off)
parkmode: .quad	0			// patched: 0 WFE, 1 WFI
