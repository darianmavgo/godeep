// efiboot: UEFI loader that boots a godeep arm64 Image on Parallels Desktop
// (docs/Parallels.md). Freestanding C: clang --target=aarch64-windows +
// lld-link, no libraries.
//
// It loads \godeep.img (make image) and \parallels.dtb (tools/efiboot/mkdtb)
// from its own volume, puts the Image at its link address, fills the tree's
// framebuffer from GOP, exits boot services and jumps to the Image with the
// MMU and caches off and x0 = the tree: the Linux arm64 boot protocol, as
// QEMU's -kernel.
//
// Without \godeep.img it is a probe: it reports the CPU, the memory map,
// GOP, Parallels' config mailbox, PCI bus 0 and every ACPI table, and
// hex-dumps the tables on the SPCR UART (docs/Parallels.md has the commands
// that turn the dump back into tables).

#include "efi.h"

static efi_system_table *ST;
static efi_boot_services *BS;
static volatile u32 *uart; // PL011 from SPCR, or 0

void *memset(void *d, int c, uintn n) {
	u8 *p = d;
	while (n--) *p++ = (u8)c;
	return d;
}

void *memcpy(void *d, const void *s, uintn n) {
	u8 *p = d;
	const u8 *q = s;
	while (n--) *p++ = *q++;
	return d;
}

static void uart_putc(char c) {
	if (!uart) return;
	while (uart[0x18 / 4] & (1 << 5)) // FR.TXFF
		;
	uart[0] = (u8)c;
}

static void puts(const char *s) {
	char16 buf[256];
	int n = 0;
	for (; *s; s++) {
		if (*s == '\n') {
			buf[n++] = '\r';
			uart_putc('\r');
		}
		buf[n++] = (u8)*s;
		uart_putc(*s);
		if (n >= 250) {
			buf[n] = 0;
			ST->con_out->output_string(ST->con_out, buf);
			n = 0;
		}
	}
	buf[n] = 0;
	if (n) ST->con_out->output_string(ST->con_out, buf);
}

static void puthex(u64 v, int digits) {
	char b[17];
	for (int i = digits - 1; i >= 0; i--, v >>= 4) b[i] = "0123456789abcdef"[v & 15];
	b[digits] = 0;
	puts(b);
}

static void putdec(u64 v) {
	char b[21];
	int i = 20;
	b[i] = 0;
	do b[--i] = '0' + v % 10; while (v /= 10);
	puts(b + i);
}

static void put4(const u8 *s) {
	char b[5] = {s[0], s[1], s[2], s[3], 0};
	puts(b);
}

static int guideq(const efi_guid *a, const efi_guid *b) {
	const u8 *x = (const u8 *)a, *y = (const u8 *)b;
	for (int i = 0; i < 16; i++)
		if (x[i] != y[i]) return 0;
	return 1;
}

static u32 rd32(const u8 *p) { return p[0] | p[1] << 8 | p[2] << 16 | (u32)p[3] << 24; }
static u64 rd64(const u8 *p) { return rd32(p) | (u64)rd32(p + 4) << 32; }

#define SYSREG(name) ({ u64 v; __asm__ volatile("mrs %0, " name : "=r"(v)); v; })

// dump prints a table as "TABLE SIG ADDR LEN" then "H " lines of 32 bytes.
static void dump(const u8 *t, u32 len) {
	puts("TABLE ");
	put4(t);
	puts(" ");
	puthex((u64)t, 16);
	puts(" ");
	putdec(len);
	puts("\n");
	if (!uart) return; // the hex goes only to the serial port
	for (u32 i = 0; i < len; i += 32) {
		char line[80];
		int n = 0;
		line[n++] = 'H';
		line[n++] = ' ';
		for (u32 j = i; j < len && j < i + 32; j++) {
			line[n++] = "0123456789abcdef"[t[j] >> 4];
			line[n++] = "0123456789abcdef"[t[j] & 15];
		}
		line[n++] = '\n';
		line[n] = 0;
		for (char *c = line; *c; c++) {
			if (*c == '\n') uart_putc('\r');
			uart_putc(*c);
		}
	}
	for (const char *c = "END\r\n"; *c; c++) uart_putc(*c);
}

static const u8 *find_table(const char *sig);

static void probe(void) {
	const u8 *spcr = find_table("SPCR");
	if (spcr && (spcr[36] == 3 || spcr[36] == 0x0e)) uart = (volatile u32 *)rd64(spcr + 44);
	puts("efiboot probe\n");
	puts("CurrentEL ");
	putdec(SYSREG("CurrentEL") >> 2 & 3);
	puts(" MIDR ");
	puthex(SYSREG("midr_el1"), 8);
	puts(" MPIDR ");
	puthex(SYSREG("mpidr_el1"), 16);
	puts(" CNTFRQ ");
	putdec(SYSREG("cntfrq_el0"));
	puts("\nPFR0 ");
	puthex(SYSREG("id_aa64pfr0_el1"), 16);
	puts(" MMFR0 ");
	puthex(SYSREG("id_aa64mmfr0_el1"), 16);
	puts(" ISAR0 ");
	puthex(SYSREG("id_aa64isar0_el1"), 16);
	puts(" SCTLR ");
	puthex(SYSREG("sctlr_el1"), 16);
	puts("\n");

	// GOP
	efi_gop *gop = 0;
	if (!EFI_ERROR(BS->locate_protocol(&GOP_GUID, 0, (void **)&gop)) && gop) {
		efi_gop_mode_info *i = gop->mode->info;
		puts("GOP ");
		putdec(i->horizontal_resolution);
		puts("x");
		putdec(i->vertical_resolution);
		puts(" stride ");
		putdec(i->pixels_per_scan_line);
		puts(" format ");
		putdec(i->pixel_format);
		puts(" fb ");
		puthex(gop->mode->frame_buffer_base, 16);
		puts(" size ");
		puthex(gop->mode->frame_buffer_size, 8);
		puts(" modes ");
		putdec(gop->mode->max_mode);
		puts("\n");
	} else {
		puts("GOP none\n");
	}
	efi_rng *rng = 0;
	puts(EFI_ERROR(BS->locate_protocol(&RNG_GUID, 0, (void **)&rng)) ? "RNG none\n" : "RNG yes\n");

	// Memory map
	static u8 map[32768];
	uintn size = sizeof map, key, dsize;
	u32 dver;
	if (!EFI_ERROR(BS->get_memory_map(&size, (efi_memory_descriptor *)map, &key, &dsize, &dver))) {
		u64 conv = 0;
		for (uintn off = 0; off < size; off += dsize) {
			efi_memory_descriptor *d = (efi_memory_descriptor *)(map + off);
			puts("MEM ");
			putdec(d->type);
			puts(" ");
			puthex(d->physical_start, 12);
			puts(" +");
			puthex(d->number_of_pages * 4096, 10);
			puts(" attr ");
			puthex(d->attribute, 16);
			puts("\n");
			if (d->type == EfiConventionalMemory) conv += d->number_of_pages * 4096;
		}
		puts("conventional MB ");
		putdec(conv >> 20);
		puts("\n");
	}

	// Parallels' config mailbox (DSDT MBOX: MVER RAM CPUM MUSB MUFS MAHC VRAM
	// MSER MHDA GPU LOON TOOL NET MTPM MPCI FLGS MGIC PINT MINT)
	volatile u32 *mbox = (volatile u32 *)0x02100000;
	puts("MBOX");
	for (int i = 0; i < 19; i++) {
		puts(" ");
		puthex(mbox[i], 8);
	}
	puts("\n");
	volatile u32 *ecam = (volatile u32 *)0x02300000;
	for (int d = 0; d < 32; d++) {
		volatile u32 *f = ecam + (d << 15) / 4;
		if ((f[0] & 0xffff) == 0xffff) continue;
		puts("PCI 00:");
		puthex(d, 2);
		puts(" id ");
		puthex(f[0], 8);
		puts(" class ");
		puthex(f[2] >> 8, 6);
		puts(" bar0 ");
		puthex(f[4], 8);
		puts(" bar1 ");
		puthex(f[5], 8);
		puts(" sub ");
		puthex(f[11], 8);
		puts("\n");
	}

	// Configuration tables, ACPI
	const u8 *rsdp = 0;
	for (uintn i = 0; i < ST->number_of_table_entries; i++) {
		efi_configuration_table *c = &ST->configuration_table[i];
		if (guideq(&c->guid, &ACPI20_GUID)) rsdp = c->table;
		if (guideq(&c->guid, &FDT_GUID)) {
			puts("FDT at ");
			puthex((u64)c->table, 16);
			puts("\n");
		}
	}
	if (!rsdp) {
		puts("no ACPI 2.0 RSDP\n");
		return;
	}
	const u8 *xsdt = (const u8 *)rd64(rsdp + 24);
	u32 n = (rd32(xsdt + 4) - 36) / 8;
	// Find SPCR first so the dump goes to the serial port.
	for (u32 i = 0; i < n; i++) {
		const u8 *t = (const u8 *)rd64(xsdt + 36 + 8 * i);
		if (t[0] == 'S' && t[1] == 'P' && t[2] == 'C' && t[3] == 'R') {
			puts("SPCR type ");
			putdec(t[36]);
			puts(" base ");
			puthex(rd64(t + 44), 16);
			puts(" irq ");
			putdec(rd32(t + 54));
			puts("\n");
		}
	}
	puts("RSDP ");
	puthex((u64)rsdp, 16);
	puts("\n");
	dump(rsdp, 36);
	dump(xsdt, rd32(xsdt + 4));
	for (u32 i = 0; i < n; i++) {
		const u8 *t = (const u8 *)rd64(xsdt + 36 + 8 * i);
		dump(t, rd32(t + 4));
		if (t[0] == 'F' && t[1] == 'A' && t[2] == 'C' && t[3] == 'P') {
			const u8 *dsdt = (const u8 *)rd64(t + 140);
			if (!dsdt) dsdt = (const u8 *)(u64)rd32(t + 40);
			puts("FADT flags ");
			puthex(rd32(t + 112), 8);
			puts(" arm_boot_flags ");
			puthex(t[129] | t[130] << 8, 4);
			puts("\n");
			dump(dsdt, rd32(dsdt + 4));
		}
	}
	puts("PROBE DONE\n");
}

static const u8 *find_table(const char *sig) {
	for (uintn i = 0; i < ST->number_of_table_entries; i++) {
		efi_configuration_table *c = &ST->configuration_table[i];
		if (!guideq(&c->guid, &ACPI20_GUID)) continue;
		const u8 *xsdt = (const u8 *)rd64((const u8 *)c->table + 24);
		u32 n = (rd32(xsdt + 4) - 36) / 8;
		for (u32 j = 0; j < n; j++) {
			const u8 *t = (const u8 *)rd64(xsdt + 36 + 8 * j);
			if (t[0] == sig[0] && t[1] == sig[1] && t[2] == sig[2] && t[3] == sig[3]) return t;
		}
	}
	return 0;
}


// --- loader ---

static void fail(const char *why) {
	puts("efiboot: ");
	puts(why);
	puts("\n");
	for (;;) BS->stall(1000000);
}

// readfile reads a whole file from the volume efiboot was loaded from.
static u8 *readfile(efi_handle image, const char16 *name, uintn *size) {
	efi_loaded_image *li;
	efi_simple_fs *fs;
	efi_file *root, *f;
	if (EFI_ERROR(BS->handle_protocol(image, &LOADED_IMAGE_GUID, (void **)&li))) fail("no loaded image");
	if (EFI_ERROR(BS->handle_protocol(li->device_handle, &SIMPLE_FS_GUID, (void **)&fs))) fail("no file system");
	if (EFI_ERROR(fs->open_volume(fs, &root))) fail("can't open the volume");
	if (EFI_ERROR(root->open(root, &f, name, 1, 0))) return 0;
	static u8 infobuf[512];
	uintn isize = sizeof infobuf;
	if (EFI_ERROR(f->get_info(f, &FILE_INFO_GUID, &isize, infobuf))) fail("file info");
	*size = ((efi_file_info *)infobuf)->file_size;
	u64 addr;
	if (EFI_ERROR(BS->allocate_pages(AllocateAnyPages, EfiLoaderData, (*size + 0xfff) >> 12, &addr))) fail("out of memory");
	uintn n = *size;
	if (EFI_ERROR(f->read(f, &n, (void *)addr)) || n != *size) fail("read error");
	f->close(f);
	return (u8 *)addr;
}

static u32 be32(const u8 *p) { return (u32)p[0] << 24 | p[1] << 16 | p[2] << 8 | p[3]; }
static void putbe32(u8 *p, u32 v) { p[0] = v >> 24; p[1] = v >> 16; p[2] = v >> 8; p[3] = v; }

// patch replaces mkdtb's placeholder cells with the framebuffer GOP set up.
static int patch(u8 *dtb, uintn size, efi_gop *gop) {
	u32 off = be32(dtb + 8), len = be32(dtb + 36);
	if (be32(dtb) != 0xd00dfeed || off + len > size) return -1;
	efi_gop_mode_info *i = gop->mode->info;
	u64 base = gop->mode->frame_buffer_base, fbsize = gop->mode->frame_buffer_size;
	u32 val[8] = {0, i->horizontal_resolution, i->vertical_resolution, i->pixels_per_scan_line * 4,
	              (u32)(base >> 32), (u32)base, (u32)(fbsize >> 32), (u32)fbsize};
	int n = 0;
	for (u32 p = off; p + 4 <= off + len; p += 4) {
		u32 v = be32(dtb + p);
		if (v >= 0xfbfb0001 && v <= 0xfbfb0007) {
			putbe32(dtb + p, val[v - 0xfbfb0000]);
			n++;
		}
	}
	return n;
}

// dc_civac cleans and invalidates [a, a+n) to the point of coherency.
static void dc_civac(u64 a, u64 n) {
	u64 line = 4 << (SYSREG("ctr_el0") >> 16 & 15);
	for (u64 p = a & ~(line - 1); p < a + n; p += line) __asm__ volatile("dc civac, %0" ::"r"(p) : "memory");
	__asm__ volatile("dsb sy" ::: "memory");
}

// jump turns the MMU and caches off (the code and stack are identity
// mapped, so execution carries on) and enters the Image.
__attribute__((noreturn)) static void jump(u64 entry, u64 dtb) {
	register u64 x0 __asm__("x0") = dtb;
	register u64 x4 __asm__("x4") = entry;
	__asm__ volatile(
		"mrs x5, sctlr_el1\n"
		"bic x5, x5, #1\n"         // M
		"bic x5, x5, #(1 << 2)\n"  // C
		"bic x5, x5, #(1 << 12)\n" // I
		"msr sctlr_el1, x5\n"
		"isb\n"
		"ic iallu\n"
		"tlbi vmalle1\n"
		"dsb sy\n"
		"isb\n"
		"mov x1, xzr\n"
		"mov x2, xzr\n"
		"mov x3, xzr\n"
		"br x4\n" ::"r"(x0),
		"r"(x4)
		: "x1", "x2", "x3", "x5", "memory");
	__builtin_unreachable();
}

#define RAM_BASE 0x40000000ULL

static void boot(efi_handle image, u8 *img, uintn isize) {
	if (isize < 64 || rd32(img + 0x38) != 0x644d5241) fail("godeep.img is not an arm64 Image");
	u64 text_offset = rd64(img + 8), image_size = rd64(img + 16);
	if (image_size < isize) image_size = isize;
	puts("efiboot: Image ");
	putdec(isize / 1024);
	puts(" KB, text_offset ");
	puthex(text_offset, 8);
	puts(", image_size ");
	putdec(image_size / 1024);
	puts(" KB\n");

	uintn dsize;
	u8 *dtb = readfile(image, u"\\parallels.dtb", &dsize);
	if (!dtb) fail("no \\parallels.dtb");
	efi_gop *gop = 0;
	if (EFI_ERROR(BS->locate_protocol(&GOP_GUID, 0, (void **)&gop)) || !gop) fail("no GOP");
	int n = patch(dtb, dsize, gop);
	if (n != 7) fail("parallels.dtb: framebuffer placeholders missing (tools/efiboot/mkdtb)");

	// The Image runs at its link address: RAM_BASE + text_offset (it
	// would relocate itself there anyway, but only from below 4 GB of
	// what it can overwrite). Claim RAM_BASE up to its end, which also
	// covers the early page tables at RAM_BASE + 0x1000.
	u64 dst = RAM_BASE, pages = (text_offset + image_size + 0xfff) >> 12;
	if (EFI_ERROR(BS->allocate_pages(AllocateAddress, EfiLoaderData, pages, &dst))) fail("RAM at 0x40000000 is taken");
	memcpy((void *)(RAM_BASE + text_offset), img, isize);
	memset((void *)(RAM_BASE + text_offset + isize), 0, image_size - isize);

	puts("efiboot: GOP ");
	putdec(gop->mode->info->horizontal_resolution);
	puts("x");
	putdec(gop->mode->info->vertical_resolution);
	puts(" at ");
	puthex(gop->mode->frame_buffer_base, 8);
	puts("; device tree ");
	puthex((u64)dtb, 12);
	puts("; exiting boot services\n");

	static u8 map[65536];
	uintn msize, key, desc_size;
	u32 ver;
	for (int tries = 0;; tries++) {
		msize = sizeof map;
		if (EFI_ERROR(BS->get_memory_map(&msize, (efi_memory_descriptor *)map, &key, &desc_size, &ver))) fail("memory map");
		if (!EFI_ERROR(BS->exit_boot_services(image, key))) break;
		if (tries == 2) fail("ExitBootServices failed");
	}
	// No firmware from here on: only the UART.
	__asm__ volatile("msr daifset, #0xf");
	// Everything the kernel reads with its caches off (Image, tree), and
	// every line of RAM it writes before turning them on (page tables, the
	// tree's copy in the DMA region), must be out of the cache: clean and
	// invalidate all of RAM.
	dc_civac(RAM_BASE, 4ULL << 30);
	dc_civac((u64)dtb, dsize);
	for (const char *c = "efiboot: jumping to the Image\r\n"; *c; c++) uart_putc(*c);
	jump(RAM_BASE + text_offset, (u64)dtb);
}

efi_status efi_main(efi_handle image, efi_system_table *st) {
	ST = st;
	BS = st->boot_services;
	BS->set_watchdog_timer(0, 0, 0, 0);
	const u8 *spcr = find_table("SPCR");
	if (spcr && (spcr[36] == 3 || spcr[36] == 0x0e)) uart = (volatile u32 *)rd64(spcr + 44);
	uintn isize;
	u8 *img = readfile(image, u"\\godeep.img", &isize);
	if (img) boot(image, img, isize);
	probe();
	for (;;) BS->stall(1000000);
}
