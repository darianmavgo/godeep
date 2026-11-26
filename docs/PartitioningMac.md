# Carving out a godeep partition next to Macintosh HD and Omarchy

This runbook sets up a bootable **godeep** entry in the Apple Silicon startup picker plus an optional `GODEEPDATA` FAT32 partition, physically between Macintosh HD and Omarchy, leaving both untouched. It was extracted from [`TargetMyMac.md`](TargetMyMac.md) §6 so the install runbook stays easy to maintain alongside the hardware and driver docs.

**Easy way:** `tools/godeep-partition.sh` automates this flow, much like the Omarchy installer. It checks the machine and shrinks Macintosh HD if there isn't already enough free space. Then it runs the Asahi installer with its menus answered for you: the OS list is cut down to a single godeep entry (the "UEFI environment only" boot chain, named `godeep`), so all you do is press Enter and type your passwords. After the reboot, `tools/godeep-partition.sh finish` adds `GODEEPDATA` and installs `bin/godeep-applem1.img` onto godeep's boot partition (with m1n1 booting godeep directly); rerun `tools/godeep-partition.sh install` (or `make native`) whenever you rebuild. The script also supports `--dry-run`, `status`, and `undo`, and every disk change asks you to type `godeep` first.

The sections below document what the script does, for reference or for running the steps by hand. (Section numbering `6.1`–`6.10` is preserved to match references in `tools/godeep-partition.sh` and other docs.)

## Contents

- [6.1 What you'll end up with](#61-what-youll-end-up-with)
- [6.2 Safety first](#62-safety-first)
- [6.3 Pre-flight checks](#63-pre-flight-checks)
- [6.4 Step 1: shrink Macintosh HD](#64-step-1-shrink-macintosh-hd)
- [6.5 Step 2: create the bootable godeep stub (Asahi installer)](#65-step-2-create-the-bootable-godeep-stub-asahi-installer)
- [6.6 Step 3 (optional): the GODEEPDATA partition](#66-step-3-optional-the-godeepdata-partition)
- [6.7 Step 4: verify](#67-step-4-verify)
- [6.8 Step 5: put godeep on its ESP and boot it](#68-step-5-put-godeep-on-its-esp-and-boot-it)
- [6.9 Undo: remove godeep and give the space back](#69-undo-remove-godeep-and-give-the-space-back)
- [6.10 Troubleshooting](#610-troubleshooting)

---

### 6.1 What you'll end up with

Before partitioning (from `diskutil list disk0`; the disk is 500.3 GB and has **no free space**):

```
disk0
 s1  Apple_APFS_ISC        524 MB   system firmware             ── never touch
 s2  Apple_APFS (disk4)  356.9 GB   Macintosh HD                ── 267.7 GB of it unused
 s3  Apple_APFS (disk2)    2.5 GB   Omarchy stub (m1n1)         ── never touch
 s4  EFI                   524 MB   EFI - OMARC                 ── never touch
 s5  Linux Filesystem      2.1 GB   Omarchy /boot               ── never touch
 s6  Linux Filesystem    132.3 GB   Omarchy root                ── never touch
 s7  Apple_APFS_Recovery   5.4 GB   system recoveryOS (last)    ── never touch
```

After (with the default 12 GB):

```
 s1  Apple_APFS_ISC        524 MB   system firmware
 s2  Apple_APFS          344.9 GB   Macintosh HD        (12 GB smaller)
 new Apple_APFS            2.5 GB   godeep stub (m1n1)  ┐
 new EFI                   500 MB   EFI - GODEE…        │ godeep, between
 new MS-DOS FAT32         ~9 GB     GODEEPDATA          ┘ macOS and Omarchy
 s3  Apple_APFS            2.5 GB   Omarchy stub
 s4  EFI                   524 MB   EFI - OMARC
 s5  Linux Filesystem      2.1 GB   Omarchy /boot
 s6  Linux Filesystem    132.3 GB   Omarchy root
 s7  Apple_APFS_Recovery   5.4 GB   system recoveryOS
```

The space comes from shrinking Macintosh HD's container, which is 75% unallocated, so the freed gap lands right after `s2` and before Omarchy's `s3`.

New partitions get the next free *entry numbers* (`disk0s8`, `disk0s9`, …) even though they sit physically between `s2` and `s3`. Always go by **name and size**, never by number alone.

**Choosing the size.** The godeep stub (2.5 GB) and its ESP (0.5 GB) need about 3 GB. The rest becomes `GODEEPDATA`, for godeep's disk driver and FAT32 filesystem work (`ReUsingGVisor.md` §5) and as a drop box shared with macOS and Omarchy. 12 GB gives about 9 GB of data space. Use 4 GB if you only want the boot entry.

### 6.2 Safety first

- **Back up first** (Time Machine or similar). Resizing APFS is routine, but it's still repartitioning the only disk in the machine.
- **Never touch** `disk0s1` (ISC), `disk0s3`–`disk0s6` (Omarchy), `disk0s7` (system recoveryOS, which must stay the last partition), or anything outside `disk0`.
- **Plug in the power adapter** for the whole process.
- **FileVault is on.** That's fine: you resize while logged in, so the volume is unlocked.
- **How to get back to macOS at any point:** shut down, then hold the power button until "Loading startup options" appears, and pick Macintosh HD. Choose "Options" in the same picker to get recoveryOS.
- **Check Omarchy still boots before you start**, so you'll know if anything changes.

### 6.3 Pre-flight checks

```sh
diskutil list disk0                         # must match the "Before" table above
diskutil apfs list disk4 | grep -E "Capacity Ceiling|Not Allocated"
#   Size (Capacity Ceiling):  356945842176 B (356.9 GB)
#   Capacity Not Allocated:   267714056192 B (267.7 GB) (75.0% free)
tmutil listlocalsnapshots /                 # only com.apple.os.update-* is fine
sudo diskutil apfs resizeContainer disk4 limits   # preview only, changes nothing
```

`limits` prints the minimum size the container can shrink to. Your target, `344.9g` with the defaults, must be above that minimum. It will be, by a very wide margin. If you see `com.backupd` (Time Machine) local snapshots, thin them first: `tmutil thinlocalsnapshots / 999999999999 4`.

### 6.4 Step 1: shrink Macintosh HD

New size = **current 356.9 GB minus your godeep size.** With the 12 GB default that's `344.9g`:

```sh
sudo diskutil apfs resizeContainer disk4 344.9g
```

`disk4` is Macintosh HD's container (the form `diskutil` prefers; it equals the physical store `disk0s2`). The resize runs live and takes a minute or two. Then run `diskutil list disk0`: a `(free space)` line of about 12 GB should appear between the Macintosh HD container and Omarchy's container. Don't create anything in it by hand: the next step needs it empty.

### 6.5 Step 2: create the bootable godeep stub (Asahi installer)

On Apple Silicon a boot entry needs **a small APFS "stub" container** holding a boot loader, registered with Apple's boot policy from recoveryOS. Doing that by hand isn't practical. Omarchy already used the Asahi installer for this, and the same tool can add a second, independent entry.

```sh
curl https://alx.sh | sh
```

This is the official Asahi Linux installer: it downloads and runs as root after you confirm, so read its prompts. The menu wording changes between versions; look for these choices:

1. **Install an OS** (not "Resize" since you already made free space; not anything that mentions Omarchy).
2. Use the **free space** it reports (about 12 GB).
3. Choose **"UEFI environment only (m1n1 + U-Boot + ESP)"**. This installs just the boot chain: no Linux, no desktop.
4. When asked for a name, enter **`godeep`**. That's the label shown in the startup picker.
5. When asked how much space to use, give it **only what it needs** (it typically suggests the minimum, about 3 GB). Leave the rest free for `GODEEPDATA`.

The installer then tells you to **shut down and finish in recoveryOS**:
- Wait 15 seconds after shutting down.
- Hold the power button until "Loading startup options" appears.
- Pick the new **godeep** entry and follow the on-screen steps. It asks for your macOS user password, to approve the boot policy for the new entry.

When it finishes, the Mac reboots into the new entry, which runs m1n1 → U-Boot.

> **For driver development:** the installer also offers **"Tethered boot (m1n1, for development)"** (option A in `TargetMyMac.md` §5; see `Using2Computers.md`). That entry waits for a second computer over USB-C, which can push kernels and run them under m1n1's hypervisor with every hardware access logged. Add it alongside the same way.

### 6.6 Step 3 (optional): the GODEEPDATA partition

Back in macOS, find the new godeep ESP: the **EFI** partition of about 500 MB that is *not* `EFI - OMARC`.

```sh
diskutil list disk0
# e.g.  9:  EFI EFI - GODEE…   500.2 MB   disk0s9     ← note this identifier
sudo diskutil addPartition disk0s9 FAT32 GODEEPDATA 0    # use YOUR godeep ESP id
```

`0` means "fill the free space". FAT32 (`fs/fat`, read & write) is shared cleanly across macOS, Omarchy, and godeep.

### 6.7 Step 4: verify

Run `diskutil list disk0` and check that:
- Macintosh HD's container is `344.9 GB`.
- A new ~2.5 GB APFS container, a ~500 MB EFI and `GODEEPDATA` appear **between** it and Omarchy's 2.5 GB container. `diskutil info <id>` shows each one's offset if you want to confirm the physical order.
- `disk0s3`–`disk0s7` are unchanged.

The startup picker (hold the power button at boot) should show **Macintosh HD, Omarchy and godeep**:
- Before installing godeep, **godeep** reaches the **U-Boot** prompt or boot menu.
- **Omarchy** still boots normally.

### 6.8 Step 5: put godeep on its ESP and boot it

The Mac image (`bin/godeep-applem1.img`, see `TargetMyMac.md` §3.1) boots to the shell under QEMU with an m1n1-style hand-off (`make check-applem1`), and boots natively on this Mac: first boot on 2026-11-25 (`screens/IMG_2341.JPG`), interactive keyboard and trackpad on 2026-11-26 (`screens/IMG_2342.JPG`, `TargetMyMac.md` §3.3).

**Easy way:** `tools/godeep-partition.sh finish`, or `tools/godeep-partition.sh install` to update godeep later. It builds `bin/godeep-applem1.img` if it's missing or stale, copies it to the godeep boot partition as `godeep.img` (keeping the previous one as `godeep.img.prev`), and **makes m1n1 boot godeep directly, without U-Boot**.

**Why not U-Boot.** The first native boot (2026-11-25) showed U-Boot autobooting **Omarchy** from the godeep entry: U-Boot's standard boot scans *every* NVMe partition and boots the first boot method that works anywhere on the disk. Omarchy's ESP (with its EFI loader) won over godeep's `extlinux.conf`. So the godeep ESP's `m1n1/boot.bin` (m1n1 stage 2 followed by its payloads: `m1n1 | dtbs | gzip(U-Boot)`) is rebuilt by `tools/m1n1boot` with `gzip(godeep Image)` in U-Boot's place. m1n1 then picks this Mac's device tree and jumps straight to godeep at EL2, using the same Image boot protocol U-Boot uses (self-relocation covers m1n1's choice of load address). U-Boot's `boot.bin` is kept as `m1n1/boot.bin.uboot`. `--uboot` puts it back and uses `extlinux.conf` autoboot. `--no-autoboot` puts it back and stops at U-Boot's prompt for the manual commands below, which are also saved as `GODEEP-BOOT.txt`.

Check a `boot.bin` by hand: `go run ./tools/m1n1boot -info boot.bin` reports whether its kernel payload is U-Boot or godeep.

**The ESP needs root to mount** (`sudo diskutil mount`). Its `diskN sN` number can **change after a reboot**: partitions added in a session get temporary identifiers, so on this Mac the godeep ESP showed as `disk0s10` right after partitioning and `disk0s4` after a reboot. Run `tools/godeep-partition.sh status` to get the current one.

**Rehearse in QEMU first: `make check-applem1`.** QEMU's `sbsa-ref` machine has RAM at 1 TB, the same base as this Mac's DRAM, so the *same* `bin/godeep-applem1.img` runs there unmodified. `tools/fakem1n1` hands it over the way m1n1 does: entry at EL2 with E2H=1, loaded away from its link address (so it self-relocates), and an m1n1-style device tree with the panel's 3456×2234 `x2r10g10b10` framebuffer. The check boots to the shell and saves the screen as a PNG. It also forces a fault at stages 1 and 3 and checks that the red fault screen appears. `make qemu-applem1` runs the same thing interactively.

What to expect on the Mac: the framebuffer clears to dark blue-grey, the boot banner and the `board:` line appear (it says `entered at EL2 (VHE, E2H=1)`), then the autorun output (`lsdev` with this Mac's devices from m1n1's tree, then `ps`, `mem`, `uptime`). About 5 s later, `lsdev idle watchdog hid` prints three rows that say whether idle sleeps, the watchdog is armed, and how far the keyboard got (`TargetMyMac.md` §3.3); then type. If the Mac reboots by itself about 30 s in, the watchdog fired: godeep stopped scheduling. If U-Boot's text or another OS appears, you didn't boot the godeep entry.

**One command does it all:** `make native` runs the host tests, builds the image, rehearses it on QEMU, then installs it (asks you to type `godeep`, and for sudo).

**If it stops before the console**, read the top-left corner of the screen. godeep draws a numbered square for each boot stage it reaches, on m1n1's framebuffer:

| Last square | Stage reached | So it stopped in |
|---|---|---|
| none (m1n1's output or the Apple logo stays) | godeep never ran Go code | `entry_arm64.s`: relocation, EL2→EL1, the early identity map; or no framebuffer in the tree |
| 1 blue | Hwinit0: Go code runs, device tree read, framebuffer mapped | building or switching to the final page tables |
| 2 green | final page tables live | Go runtime setup (`schedinit`: the heap), or TamaGo's `CPU.Init` |
| 3 yellow | Hwinit1: CPU, caches, timers | runtime start: goroutines, `main`, the kernel's init |
| 4 purple | Go runtime up, board setup started | console creation (`drivers/simplefb`) |

A **red screen** titled `GODEEP EARLY FAULT` means the CPU took an exception. Take a photo: it shows the stage plus **ESR** (why: e.g. `96000004` is a data abort, translation fault), **ELR** (the faulting instruction: look it up with `go tool objdump` on an unstripped build) and **FAR** (the address it touched). Then a second computer with m1n1's hypervisor (`Using2Computers.md`) can show the rest. Hold the power button to get back.

With U-Boot restored (`--no-autoboot`), boot the godeep entry and, at the U-Boot prompt:

```
=> load nvme 0:<godeep-ESP-partition> ${kernel_addr_r} godeep.img
=> booti ${kernel_addr_r} - ${fdtcontroladdr}
```

U-Boot numbers partitions by GPT entry; `part list nvme 0` shows them. The godeep ESP is the 500 MB EFI partition right before `GODEEPDATA`.

### 6.9 Undo: remove godeep and give the space back

1. Find the godeep stub container (the ~2.5 GB APFS container that is **not** Omarchy's `disk2`):

   ```sh
   diskutil apfs list | grep -B2 -A12 "godeep"
   ```

2. Delete godeep's three partitions. **Double-check each identifier is godeep's, not Omarchy's:**

   ```sh
   sudo diskutil apfs deleteContainer <godeep stub container, e.g. disk7>
   sudo diskutil eraseVolume free none <godeep ESP, e.g. disk0s9>
   sudo diskutil eraseVolume free none <GODEEPDATA, e.g. disk0s10>
   ```

3. Grow Macintosh HD back into the freed space:

   ```sh
   sudo diskutil apfs resizeContainer disk4 0     # 0 = grow to fill
   ```

The godeep entry disappears from the startup picker once its stub container is gone.

### 6.10 Troubleshooting

| Problem | What to do |
|---|---|
| `resizeContainer` refuses ("not enough free space", "snapshots") | Run `limits` again; thin Time Machine local snapshots (§6.3); reboot and retry, since a pending macOS update can pin space |
| The installer says there's no free space | Make sure Step 1 left a `(free space)` gap (`diskutil list disk0`), and that you didn't create a partition in it |
| No "godeep" in the startup picker | The recoveryOS step didn't complete. Boot it from the picker ("Options" → the godeep entry) and finish the installer's prompts |
| Booting godeep shows U-Boot, then Omarchy | U-Boot is still on the godeep ESP, and its disk-wide scan found Omarchy first. Run `tools/godeep-partition.sh install` (m1n1 boots godeep directly) |
| Booting godeep stays on the Apple logo or m1n1's output | godeep stopped before its console came up (§6.8) |
| Omarchy stopped booting | Nothing here touches its partitions. Pick Omarchy explicitly in the startup picker. If it still fails, its own recovery steps apply; your backup covers the rest |
| Want macOS back immediately | Hold the power button → pick Macintosh HD |
