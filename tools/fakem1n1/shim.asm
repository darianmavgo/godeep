// fakem1n1 shim: the part of m1n1's hand-off that matters to godeep, for
// QEMU sbsa-ref. QEMU starts it at EL3 (-device loader,cpu-num=0). It:
//   1. enables the PL011 and prints "m1n1-shim EL3"
//   2. drops to EL2 with the HCR_EL2 value from `hcr` (E2H=1 like Apple cores)
//   3. prints "EL2 E2H=<0|1>" (what the CPU kept), then jumps to the kernel
//      Image with the device tree in x0, x1-x3 = 0 (Linux arm64 boot protocol)
//
// Assembled with Apple clang (`clang -c -x assembler -arch arm64 shim.asm`); the machine
// words are embedded in shim_code.go. Regenerate them with `go generate`
// (gen_shim.go) after changing this file. The four parameter words at the end
// are patched by fakem1n1: keep them last and in this order.

	.text
	.globl	_start
	.p2align 2
_start:
	ldr	x9, uart
	mov	w10, #0x301		// UARTCR: UARTEN | TXE | RXE
	str	w10, [x9, #0x30]
	adr	x11, msg_el3
	bl	puts

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
