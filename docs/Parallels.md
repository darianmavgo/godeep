# godeep on Parallels Desktop

The arm64 godeep Image (the same `bin/godeep-arm64.img` QEMU boots) runs
under Parallels Desktop 27 on this Mac, on all of the VM's cores, with a
framebuffer console (USB keyboard and trackpad through the VM window), a
serial console, and the network: DHCP on Parallels' shared network, ssh/scp
from the Mac straight to the VM's address, the internet through Parallels'
NAT (DNS, NTP, HTTPS). First booted 2026-11-27 on Parallels Desktop 27.0.3
(VM firmware 27.1.0 (58756)).

```
make parallels-setup   # once: boot disk, serial socket, boot order on the VM (PD_VM=GoDeep)
make parallels         # build, copy to the boot disk, (re)start the VM
nc -U bin/parallels/serial.sock   # serial console (the shell is on the VM window too)
make ssh-parallels [CMD='"lsdev net0"']   # ssh to the VM's address (from Parallels' DHCP leases)
make check-parallels   # boot + GOMAXPROCS + all cores + parallel burn + DHCP + ssh (PD_CPUS=4)
```

Needs a Parallels license (trial is fine; `prlctl` needs Pro), `brew install
lld mtools`, and an arm64 VM of type "Other" with **4 GB of RAM** (board/virt
uses RAM 0x40000000-0x13fffffff, see below). The VM's own disk can stay.

Measured (4 cores, M1 Pro host): `burn 1 5` 2374 MB/s, `burn 4 10` 8841
MB/s (3.7x), the VM process at 4.0 host cores during it; idle about 16% of a
host core (QEMU HVF: ~2%; not investigated).

## How it boots

Parallels has no `-kernel`: a VM always starts in its UEFI firmware (EDK2
style), which describes the machine with ACPI only. So:

- **`tools/efiboot`** is a UEFI application (`BOOTAA64.EFI`, freestanding
  C, `clang --target=aarch64-windows` + `lld-link`; ~450 lines with `efi.h`).
  It reads `\godeep.img` and `\parallels.dtb` from its own FAT volume,
  copies the Image to its link address (RAM + `text_offset` = 0x4000f000,
  claimed with `AllocateAddress`), fills the tree's framebuffer from GOP,
  exits boot services, cleans and invalidates all 4 GB of RAM to the point
  of coherency, turns the MMU and caches off and jumps with x0 = tree: the
  Linux arm64 boot protocol, what QEMU's `-kernel` does. Without
  `\godeep.img` it is a **probe**: CPU registers, memory map, GOP,
  Parallels' config mailbox, PCI bus 0, every ACPI table summarized, and
  hex-dumped on the SPCR UART (see [Re-deriving](#re-deriving-the-tree)).
- **`tools/efiboot/mkdtb`** writes the device tree from what the ACPI tables
  say (`kernel/fdt.Builder`). The framebuffer cells are placeholders
  (`0xfbfb000N`) the loader replaces with GOP's values; the rest is fixed
  per VM configuration (`-cpus`, `-mem`).
- **The boot disk** is a Parallels *plain* disk (`prl_disk_tool create`):
  the `.hds` inside the `.hdd` bundle is a raw image, so `mformat`/`mcopy`
  write it directly (whole-disk FAT32, no partition table; the firmware
  accepts it).

## What the VM has (Parallels 27.1, from its ACPI tables)

| | Parallels arm64 VM | godeep uses |
|---|---|---|
| CPU | EL1 (no EL2), MIDR 0x410f0000, MPIDR 0..N-1 (Aff0) | yes |
| RAM | 0x40000000 + VM size, one range (UEFI: ACPI NVS at 0xffff0000) | 0x40000000-0x13fffffff: identical to QEMU virt `-m 4G` |
| PSCI | over **SMC** (FADT arm_boot_flags = compliant, not HVC) | CPU_ON, SYSTEM_OFF |
| GIC | v3: GICD 0x02010000, GICR 0x02500000 (0x20000 per core), GICv2m MSI frame 0x02250000 | yes (no MSI) |
| Timer | generic timer, 24 MHz, virtual timer PPI 27 | yes (idle governor) |
| UART | PL011 COM1 0x02110000 (GSI 32, the SPCR console), COM2-4 0x02220000-0x02240000 | COM1 = serial0 |
| Display | UEFI GOP 1024x768 BGRX at 0x20000000 (3 MB); DSDT GPU0 `PRL4005` (Parallels video) | GOP as simple-framebuffer |
| PCI | ECAM 0x02300000, bus 0 only; windows 0x02400000 (1 MB), 0x10000000 (256 MB); BARs firmware-assigned | scanned (no drivers bound yet) |
| PCI devices | 00:01 Intel HDA, 00:02 Intel EHCI, 00:03 NEC xHCI `1033:0194`, 00:05 virtio-net **legacy** `1af4:1000` (BAR0 memory, MSI-X), 00:09 Parallels tools `1ab8:4000` | — |
| Platform devices | AHCI `PRL4010` 0x02140000 (GSI 34); virtio-mmio (`LNRO0005`) slots for net/balloon/GPU at 0x021c0000.. (enabled per the mailbox) | — |
| Config mailbox | 0x02100000: MVER RAM CPUM MUSB MUFS MAHC VRAM MSER MHDA GPU LOON TOOL NET MTPM MPCI FLGS MGIC PINT MINT (DSDT `MBOX`); `_STA` methods read it | read by the probe |
| RTC | ACPI000E time/alarm device at 0x02121000 (not a PL031) | no (clock starts at 1970) |
| Power button | ACPI GED (GSI 48, 117) | no: `halt` uses PSCI; `prlctl stop --kill` |
| RNG | no EFI_RNG_PROTOCOL, no virtio-rng | DRBG seeded from the counter (insecure, as board/applem1) |

On this VM config (net0 virtio, USB 3.0 on) the mailbox has NET = 0 and
MPCI = 3: the NIC is the legacy virtio-pci function, not virtio-mmio.

## Kernel changes (all in board/virt; QEMU unchanged)

- `board/virt/early.go`: Hwinit1 takes the UART, GIC, RTC (absent = none),
  PSCI conduit and framebuffer from the tree with the allocation-free
  `fdt.ScanEarly` (which gained `GICD`/`GICR`, `RTC` and `PSCI`). The
  defaults stay QEMU virt's. GIC addresses are variables now.
- `board/virt/mmu.go`: a framebuffer below RAM is mapped Normal
  non-cacheable (as Device memory, Go's stores fault); Hwinit0 also points
  the UART at the tree's before anything can print.
- `board/virt/psci*.go`: CPU_ON over SMC when the tree says so.
- The console UART's IRQ and the framebuffer console come from the tree.

## Gotchas found

- **A guest access to an address nothing answers stops the VM** (not a
  data abort): Parallels aborts with `PRL_ERR_VM_MONITOR`. So does any
  exception taken before godeep installs its vectors (it goes to the stale
  UEFI vectors: `UEFI-exception-NULL`). Nothing may touch QEMU's addresses
  (PL011 0x09000000, PL031) on this board: everything comes from the tree.
- **Debugging:** `~/Parallels/GoDeep.pvm/parallels.log` has the full
  register dump of an abort (ELR, ESR, FAR, X0-X30, LR); symbolize with an
  unstripped build: `go build` without `-s -w`, then `go tool addr2line`.
  The first Parallels boot died this way: `pgtable.Add` rejected an
  out-of-order region and the error path concatenated a string before the
  heap existed.
- `prlctl capture VM --file x.png` screenshots the VM window.
- Closing the VM window suspends it by default, and a suspended VM can't be
  reconfigured: parallels-setup sets `--on-window-close keep-running`.
- The first serial port works (a forum report said ttyAMA0 stays silent
  under Linux; not so here).

## Network

Whatever adapter type the VM is given (`virtio`, `e1000`, `e1000e`, `rtl`),
an arm64 VM gets the same NIC: PCI 00:05, legacy virtio-net `1af4:1000`
(the config mailbox keeps the virtio-mmio NIC slots off). godeep drives it
with `drivers/virtio/pci` (the legacy transport: an 8/16/32-bit register
block in BAR0, `mmio.NarrowRegs`; one page-aligned block per queue,
`vmmio.NewLegacyQueue`) under the same virtio-net driver as QEMU's
virtio-mmio (`net.PCIDriver`; 10-byte header without VERSION_1). Polled
(2 ms busy / 20 ms idle): INTx isn't routed. Host-tested against a legacy
device model that flags any access of the wrong width
(`drivers/virtio/pci/legacytest`).

Parallels' shared network: the VM gets 10.211.55.x from Parallels' DHCP
(10.211.55.1 is gateway and DNS); the Mac is 10.211.55.2 on `bridge100`
and reaches the VM directly, so sshd needs no port forwarding.
`/Library/Preferences/Parallels/parallels_dhcp_leases` maps the VM's MAC to
its address (make ssh-parallels). The first attempt once failed with "No
route to host" and an incomplete ARP entry on the Mac; with a fresh address
the Mac resolved the VM at once, so it looked like a stale entry, not a
guest problem.

## Not done (by size)

- **Disk**: AHCI (platform, `PRL4010`): a new driver. Files can come in
  over the network (`mount9p`) once there is one.
- RTC (`ACPI000E` registers unknown; NTP sets the clock once the network
  is up), power button (GED), RNG.
- The tree is static per VM configuration (cores, RAM); efiboot could
  check MADT's core count against it.

## Re-deriving the tree

If a Parallels update changes the hardware, boot the probe (delete
`godeep.img` from the boot disk, or `mdel -i .../*.hds ::/godeep.img`),
point serial0 at a file (`prlctl set VM --device-set serial0 --output
FILE`), and rebuild the tables (`brew install acpica`):

```
python3 - FILE <<'EOF'   # TABLE SIG ADDR LEN / H hex... / END  ->  SIG.dat
import sys
cur=None
for l in open(sys.argv[1], errors="replace"):
    l=l.strip()
    if l.startswith("TABLE "): cur, data = l.split()[1], b""
    elif l.startswith("H ") and cur: data += bytes.fromhex(l[2:])
    elif l == "END" and cur: open(cur+".dat","wb").write(data); cur=None
EOF
for f in *.dat; do iasl -d $f; done   # APIC.dsl (MADT) FACP GTDT MCFG SPCR DSDT ...
```

## Sources

- The probe's dump of this VM's ACPI tables (above).
- RenkuOS, [virtio_pci: legacy registers in a memory BAR (#560)](https://github.com/RenkuOS/Source/pull/560) and [virtio_gpu: attach backing as Parallels Desktop expects (#567)](https://github.com/RenkuOS/Source/pull/567): the legacy virtio-net, and a virtio-gpu descriptor quirk (Parallels' virtio-gpu wasn't enabled on this VM).
- Parallels developer guide: [virtual serial port](https://docs.parallels.com/parallels-desktop-developers-guide/command-line-interface-utility/manage-virtual-machines-from-cli/virtual-machine-configuration-tasks/device-management/virtual-serial-port).
