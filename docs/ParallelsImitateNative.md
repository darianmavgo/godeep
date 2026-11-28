# Making Parallels imitate a native boot

Status: proposal (2026-11-28). Nothing here is built yet. Each change says
which files it touches and how to check it.

A native boot of `bin/godeep-applem1.img` on this MacBook Pro flickered the
console on, then crash-looped. Parallels is the fast loop: a boot takes
seconds, ssh works, and nothing needs reinstalling. But today a Parallels
boot differs from a native one in policy (cores, autorun, desktop,
bootargs) and in resources. This document lists every difference, what to
change to remove it, and what can't be removed.

Decisions behind it:

- Parallels gets **half of the real hardware** by default. The Mac image
  gets **all of it**.
- Parallels **keeps its network** (ssh is the debugging lifeline);
  `godeep.nonet` gives a run that matches the Mac exactly.

## 1. What "identical" can mean

Parallels can't boot the Mac image itself. That image:

- is linked for DRAM starting at physical address `0x100_0000_0000`.
  That's the "1 TB": where the M1's memory sits in the address map, not
  how much of it there is. This Mac has 16 GB.
- enters at EL2 with VHE (E2H=1) from m1n1. Parallels' UEFI hands over
  at EL1.
- talks to Apple hardware: AIC2 interrupts, the spin-table core release,
  the SPI keyboard, ANS NVMe, the SMC, the watchdog, AGX.

So Parallels keeps running `board/virt`. "Identical" means:

1. the same **boot policy**: what's on at boot, what the shell types,
   what you see first, and which bootargs change it;
2. **resources in the same proportion** (half of each);
3. **failures that look the same**: the same boot log and fault screen;
4. a written list of the differences that remain (section 6), so a bug
   seen only on the Mac points straight at them.

## 2. Resource targets

| | Real Mac | Mac image today | Mac image target | Parallels today | Parallels target |
|---|---|---|---|---|---|
| Cores | 10 (8 P + 2 E) | 1 at boot; 10 after `enable smp` | unchanged | 4, all at boot | **5**; 1 at boot, 5 after `enable smp` |
| RAM | 16 GB | 4 GB Go heap + 512 MB DMA (`board/applem1/mem.go`) | **sized from `/memory`**: everything usable above the low 4 GB, minus carve-outs, DMA and pstore | 4 GB (3.5 GB heap + 512 MB DMA, `board/virt/mem.go`) | **8 GB** |
| Display | 3456x2234, x2r10g10b10 | same | same | 1024x768, x8r8g8b8 | **1728x1117** (the panel's default "looks like" size: half in each axis) |
| Network | none, unless a USB Ethernet adapter | none (`NetWait: -1`) | unchanged | virtio-net, DHCP, ssh | unchanged; `godeep.nonet` turns it off |
| GPU | AGX | off; `enable agx` | unchanged | none | none (section 4.7) |
| Disk | ANS NVMe, GODEEPDATA | read-only (`ssdWrites = false`) | unchanged | none (no AHCI driver) | unchanged for now (section 6) |

## 3. Changes to the Mac image: match the real hardware

### 3.1 RAM from the device tree

Today `board/applem1/mem.go` hard-codes `RAM_START = DRAM_BASE + 4 GB`,
`RAM_SIZE = 4 GB` and `DMA_SIZE = 512 MB`, so about 11 GB of the 16 sit
unused. `checkMemory` (`board/applem1/devices.go`) only *checks* that
fixed window against the tree.

Change:

- In `Hwinit0` (before the runtime reads `goos.RamSize`), scan `/memory`
  and `/reserved-memory` with `fdt.ScanEarly`, which is allocation-free.
  This needs it to report those ranges.
- Keep `RAM_START` at DRAM + 4 GB (iBoot, m1n1 and the kernel's load area
  are low). End the heap below the first carve-out at the top: the
  framebuffer, firmware regions and m1n1's reserved ranges.
- Put the DMA region and pstore at the end of the heap rather than at
  fixed addresses. pstore must still land at the **same address every
  boot** to survive a reset, so derive it from the top of usable RAM. That
  address is stable for one machine.
- `kernel/pgtable` maps the larger range (the page-table pool may need to
  grow).
- `goos.HeapAddrBits` 41 (third_party/tamago/GODEEP_PATCHES.md) still
  covers it: 1 TB + 16 GB < 2 TB.
- `checkMemory` turns from "is the fixed window usable" into "what we
  took", shown in the banner.

Risk: the heap-arena metadata grows with the heap size. Measure the early
boot cost on fakem1n1 (it already runs `-m 16G`).

### 3.2 Cores and display

These already match the hardware: all 10 cores are described, the
framebuffer is m1n1's 3456x2234. SMP stays **opt-in** (`enable smp`,
`godeep.smp`) until the 2026-11-27 boot hang is explained.

## 4. Changes to Parallels: imitate a native boot

### 4.1 Bootargs on board/virt

`board/virt` never reads `/chosen/bootargs`. `fdt.ScanEarly` already
returns `Bootargs`, and `tools/efiboot/mkdtb -bootargs` already writes them.

- Move `bootArg` (`board/applem1/devices.go`) into `kernel/fdt` (a word
  match over `/chosen/bootargs`, host-tested), and use it from both
  boards.
- `make parallels BOOTARGS=...` passes `$(BOOTARGS)` to `mkdtb -bootargs`.
  It's the same variable `make native` hands to `tools/m1n1boot
  -bootargs`, so one command line means the same thing on both.

### 4.2 Native mode only when asked

QEMU virt (`make qemu`, `check-virt`, `check-smp`) must keep today's
behaviour. `make parallels` adds `godeep.native` to the bootargs by
default (`PD_BOOTARGS ?= godeep.native`), and `board/virt` applies the
Mac's policy (4.3 to 4.5) only when it sees it.

### 4.3 SMP off by default

- Under `godeep.native`, `NewBoard` (`board/virt/board.go`) skips
  `startSMP` unless `godeep.smp` is given. `SMPInfo` then reads
  `1 of 5 cores (smp off: ...)`, like the Mac.
- A `features` device on board/virt (as in `board/applem1/features.go`)
  offers `smp`. Its `Enable` runs `startSMP` at run time:
  - once only (a `CompareAndSwap` guard);
  - `runtime.Gosched()` in the one-second alive wait;
  - waking the redistributors and `CPU_ON` work the same after boot.
    The comment that it "runs before anything else is scheduled" has to
    become true or be removed. Check `wakeSGI` enabling on the boot core
    with interrupts already live.
- `enable` then lists `smp` on Parallels. `agx` isn't offered there.

### 4.4 Same autorun and desktop

Move `Autorun` and `Desktop` out of `board/applem1/board.go` into a
shared place (for example `kernel.NativeAutorun`), so both boards type
the same lines. On Parallels, the Mac-only device names in
`lsdev idle watchdog hid usb nvme smc rtc kbdlight` just print nothing,
because lsdev filters by name prefix. Then:

- `enable` (lists what's off), then `desktop`;
- `godeep.nodesktop` skips the desktop, on both.

board/virt's console input (`consoleIn`) is the same kind of byte channel
as the Mac's `in`, so autorun is a prefill of that channel. That's what
the Mac does.

### 4.5 Network: on, with an off switch

Keep virtio-net, DHCP and ssh. `godeep.nonet` sets `kernel.Board.NetWait`
to "no network" and skips the NIC search, so a run can match the Mac's
"no network" boot exactly (and show any code that assumes a network).

### 4.6 Resources

- **Cores:** `PD_CPUS ?= 5` (Makefile). It feeds both `prlctl set --cpus`
  (`parallels-setup`) and `mkdtb -cpus`.
- **RAM:** `parallels-setup` sets `--memsize 8192`. board/virt's window is
  fixed at 4 GB today (`board/virt/mem.go`: `ramSize` 3.5 GB, DMA in the
  last 512 MB). Size it from `/memory` with the same code as 3.1 (DMA and
  pstore at the top). `mkdtb` writes `/memory` for the VM's size. QEMU's
  `-m 4G` keeps working because the size comes from the tree.
- **Display:** `parallels-setup` turns `--high-resolution on` and asks for a
  1728x1117 guest resolution if Parallels allows a custom one.
  `tools/efiboot` walks GOP `QueryMode`, picks the mode closest to
  1728x1117, calls `SetMode` before patching the tree's framebuffer cells,
  and logs what it got. The pixel format stays 8-bit (x8r8g8b8) against the
  Mac's 10-bit. `simplefb` handles both, and the desktop scale is
  `max(1, width/1600)` = 1 on both at these sizes.

### 4.7 AGX

There's no Apple GPU to emulate. Parallels' `enable` doesn't list `agx`.
GPU bugs can only be found on the Mac. Bisect there with `enable agx`
after a clean default boot.

## 5. Next: failures that look the same

The Mac shows numbered stage squares, a live boot log under them, and a
red fault screen (ESR/ELR/FAR) before its console exists. It keeps the boot
log across a warm reset (`ram:/bootlog.prev.txt`). Parallels shows nothing
until the console is up. Port:

- the stage squares and fault screen (`board/applem1/early.go`,
  `early_arm64.s`; built on `fdt.ScanEarly` + `simplefb.Early`, which
  board/virt already uses for the framebuffer);
- the boot log breadcrumbs (`board/applem1/bootlog.go`, format in
  `kernel/pstore/bootlog.go`) into board/virt's pstore MB.

To check first: whether Parallels keeps guest RAM across a reset (QEMU
does; check-pcie depends on it). If it doesn't, the boot log still helps
within a boot, and `godeep/crash.txt` needs a disk.

## 6. Differences that stay

| | Mac | Parallels | Why it stays / what covers it |
|---|---|---|---|
| Entry | EL2, VHE, from m1n1 | EL1, from UEFI (tools/efiboot) | Parallels gives no EL2. fakem1n1 rehearses the EL2 drop |
| Core release | m1n1 spin table + SEV | PSCI `CPU_ON` | different firmware. fakem1n1 rehearses the spin table (`-park wfi`, `-secmmu`) |
| Interrupts | AIC2, polled every 1 ms | GICv3, real IRQs | different hardware. A `godeep.pollirq` mode on virt could copy the polling |
| Idle | WFE + event stream (~1,500 wake-ups/s) | WFI + virtual timer | optional `godeep.wfeidle` on virt to copy the Mac's idle timing |
| Watchdog | Apple WDT, 30 s | none | a software watchdog could stand in; not proposed |
| Input | SPI HID keyboard + multitouch trackpad | USB HID keyboard, tablet, mouse | different devices. Both feed the same console channel and `simplefb.Mouse` |
| Storage | ANS NVMe, GODEEPDATA | none yet | needs an AHCI driver (docs/Parallels.md, "Not done") |
| Entropy | 72 bytes of /chosen seeds | none | Parallels has no RNG device in the tree |
| Addresses | DRAM at 0x100_0000_0000 | DRAM at 0x40000000 | linked in; only matters to code with addresses built in |
| Pixel format | 10-bit | 8-bit | GOP offers 8-bit only |
| Toolchain | bin/tamago-go (patched runtime) | same | when comparing runs, note which toolchain built each image |

## 7. Which loop for which bug

- **Parallels:** everything above the board: shell, desktop, wm, network,
  SMP scheduling, memory pressure at 8 GB.
- **fakem1n1** (`make check-applem1`): the Mac's boot path: m1n1 handoff,
  EL2 drop, relocation, Apple device tree, spin-table release, fault
  screen.
- **The Mac:** hardware. Bisect from the default image (1 core, no AGX,
  desktop): if it reaches the desktop, `enable smp`, then `enable agx`.

## 8. Checking each change

- `make check-parallels`, extended:
  - `cat proc:/version` shows `GOMAXPROCS=1`;
  - `enable` lists `smp` as off;
  - the desktop is up from autorun (`ps` shows `term1`; screenshot);
  - `enable smp` gives `5 of 5 cores`;
  - `mem` shows about 8 GB from the system;
  - `make parallels BOOTARGS=godeep.nonet`: `ifconfig` shows no interface.
- `make check-virt` and `make check-smp` unchanged (no `godeep.native` on
  QEMU).
- `make check-applem1` after 3.1: `mem` reports about 11 GB more at
  fakem1n1's `-m 16G`, and every existing step still passes.
- Then a native boot: the banner's memory line, and `mem`.
