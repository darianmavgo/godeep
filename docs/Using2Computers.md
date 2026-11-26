# Using two computers: the Windows PC drives, the Mac is the target

This is a tutorial for a two-machine godeep setup:

- **The MacBook Pro (M1 Pro, J316s)** is the **target**. It boots godeep through m1n1, as described in `docs/TargetMyMac.md` §5 and `docs/PartitioningMac.md`.
- **The Windows 11 gaming PC** is the **dev host**. You edit, build, test and run QEMU there, and it pushes each new kernel to the Mac over a USB-C cable.

The goal is a shorter loop between an edit and seeing the result on real hardware, with **text output** instead of photos of the screen.

Some pieces don't exist yet. They're marked **(to add)**, and §9 lists them. Commands for m1n1's `proxyclient` change between versions, so check each tool's `--help` the first time you use it.

## Contents

- [TL;DR](#tldr)
- [1. Why two computers](#1-why-two-computers)
- [2. The setup at a glance](#2-the-setup-at-a-glance)
- [3. One-time setup on the Windows PC](#3-one-time-setup-on-the-windows-pc)
- [4. One-time setup on the Mac](#4-one-time-setup-on-the-mac)
- [5. The dev ladder: four loops, fastest first](#5-the-dev-ladder-four-loops-fastest-first)
- [6. Getting output back from the Mac](#6-getting-output-back-from-the-mac)
- [7. When godeep hangs: getting back to m1n1](#7-when-godeep-hangs-getting-back-to-m1n1)
- [8. Keeping the code in sync](#8-keeping-the-code-in-sync)
- [9. Repo changes that make this smooth (to add)](#9-repo-changes-that-make-this-smooth-to-add)
- [10. Troubleshooting](#10-troubleshooting)
- [11. Cheat sheet](#11-cheat-sheet)

---

## TL;DR

1. On the PC: install **WSL2 (Ubuntu)** and clone the repo **inside WSL**, not under `/mnt/c`. Install Go, TamaGo, QEMU and m1n1's `proxyclient` there. `make test` and `make check-applem1` work as they do on the Mac.
2. Install **usbipd-win** so that WSL can see the Mac's USB serial ports when m1n1 is in proxy mode.
3. On the Mac: add an m1n1 entry that **waits for the PC** (proxy mode) and make it the **default startup disk**.
4. Connect the two with a **USB-C data cable**. From WSL, each run is: `make image BOARD=applem1`, then push it with m1n1's `linux.py` (native speed) or `run_guest.py` (under m1n1's hypervisor, with every MMIO access logged and godeep's UART shown in a terminal).
5. When godeep hangs, the 30 s watchdog reboots the Mac into the proxy entry, ready for the next push. You don't have to touch the Mac.

The first driver to write for this setup is **`drivers/apple/uart`** (the Samsung UART, about 100 lines). Under the hypervisor, its output is forwarded to the PC as a serial port, so the Mac's console becomes text in a terminal (§6).

---

## 1. Why two computers

Right now the Mac is both the dev machine and the target. One native test looks like this:

| Step | Where | Rough time |
|---|---|---|
| Edit, `make native` (tests, image, `check-applem1`, install with sudo) | macOS | 1–2 min |
| Shut down, hold power, pick **godeep** | Mac | ~1 min |
| Read the screen, take a photo | Mac | — |
| Hold power, pick **Macintosh HD**, log in, reopen the editor | Mac | 1–2 min |

That's **4–5 minutes per try**. The only output is the framebuffer. Once godeep is running, macOS isn't, so you can't look at logs, compare against the source, or ask Claude about what you see.

With a second computer, macOS stays off the critical path:

| Step | Where | Rough time |
|---|---|---|
| Edit, `make image BOARD=applem1` | PC (WSL) | 10–20 s |
| Push the ~12 MB Image over USB and jump to it | PC → Mac | a few seconds |
| Read the serial output, MMIO trace and fault dump in a terminal | PC | — |
| Reboot back to m1n1 (watchdog, or a command from the PC) | Mac | ~20–30 s |

That's under a minute, and **nothing is written to the Mac's disk** during the loop. The times are estimates, so measure your own. The bigger win isn't speed: it's that output arrives as text on the machine where you work.

**Why the PC is the host, and not the other way around.** m1n1's proxy is a USB device that runs on the *target*. Any computer with Python and a USB port can drive it. The PC can do every other job too (build, test, QEMU). The Mac can't be both, because while it runs godeep, macOS isn't running.

---

## 2. The setup at a glance

```
 Windows 11 PC (dev host)                              MacBook Pro (target)
 ┌──────────────────────────────────────┐             ┌──────────────────────────────┐
 │ Windows: VS Code, browser, usbipd-win│             │ iBoot                        │
 │ ┌──────────────────────────────────┐ │  USB-C data │  → m1n1 (proxy mode, waits)  │
 │ │ WSL2 Ubuntu                      │ │    cable    │      ↑ commands over USB     │
 │ │  ~/godeep (git clone)            │◄├─────────────┤►     → godeep (native),      │
 │ │  go, tamago-go, qemu             │ │             │        or godeep as a guest  │
 │ │  m1n1/proxyclient (Python)       │ │             │        under m1n1's HV       │
 │ │  /dev/ttyACM0  proxy commands    │ │             │                              │
 │ │  /dev/ttyACM1  guest's UART (HV) │ │             │ macOS / Omarchy: untouched   │
 │ │  Claude Code                     │ │             │ (pick them with the power    │
 │ └──────────────────────────────────┘ │             │  button when you want them)  │
 └──────────────────────────────────────┘             └──────────────────────────────┘
```

When m1n1 is in proxy mode, it shows up on the PC as a USB device with **two serial ports** (CDC-ACM). The first carries proxy commands: read and write memory, load a payload, jump to it, reboot. The second is for the hypervisor's virtual UART: anything the guest writes to the Mac's UART comes out there.

What runs where:

| Job | Machine | Notes |
|---|---|---|
| Editing, git, Claude Code | PC (WSL) | VS Code with the **WSL** extension, so the editor runs on Windows and the files and tools live in Linux |
| `make test`, `-race -count=50` | PC (WSL) | Pure Go. A desktop CPU with more cores helps with `-count=50` |
| `make check-applem1`, `make qemu-applem1` | PC (WSL) | Already TCG on the Mac, so roughly the same story on x86 |
| `make qemu` (`board/virt`) | PC (WSL), `ACCEL=tcg` | No HVF on Windows. arm64 under TCG is slower than the Mac's HVF but fine for the shell. For HVF-specific work (idle cost, the HVF WFI bug) use the Mac in macOS |
| `make image BOARD=applem1` | PC (WSL) | Cross-compiling is built into Go: the same bytes as on the Mac |
| Pushing and running on hardware | PC → Mac | m1n1 `proxyclient` tools |
| `make native` (installing a standalone boot) | Mac (macOS) | Uses `diskutil` and the ESP. Only for milestones (§5, loop 4) |

---

## 3. One-time setup on the Windows PC

> **Automated setup:** [`tools/setup-windows.ps1`](../tools/setup-windows.ps1) automates §3.1–§3.5 (WSL2 + Ubuntu, `usbipd-win`, Python + m1n1 `proxyclient`, `M1N1DEVICE`, `dialout`, host Go + TamaGo Go, cloning `~/godeep`, and installing a `godeep-push` helper):
> ```powershell
> powershell -ExecutionPolicy Bypass -File tools\setup-windows.ps1 [-NoBuild]
> ```
> Use the manual steps below if you prefer to set up each component by hand or want to add QEMU inside WSL as well.

### 3.1 WSL2 with Ubuntu

In an **administrator** PowerShell:

```powershell
wsl --install -d Ubuntu
```

Reboot if it asks, then open "Ubuntu" from the Start menu and create a user.

**Keep the repo inside the Linux filesystem** (`~/godeep`, which is `\\wsl$\Ubuntu\home\<you>\godeep` from Windows). Under `/mnt/c/...` every file access crosses a 9P bridge, and `go build` and `git status` become several times slower.

### 3.2 Toolchains inside WSL

```sh
sudo apt update
sudo apt install -y build-essential git make python3 python3-pip python3-venv \
    qemu-system-arm qemu-system-x86 e2fsprogs dosfstools mtools

# Host Go: use the official tarball (apt's Go is usually too old for this repo's go.mod).
# Pick the version go.mod asks for from https://go.dev/dl/
curl -LO https://go.dev/dl/go1.27.4.linux-amd64.tar.gz      # match go.mod
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.27.4.linux-amd64.tar.gz
echo 'export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH' >> ~/.bashrc && . ~/.bashrc

# TamaGo Go: same tag as on the Mac (CLAUDE.md, Toolchain setup).
git clone --depth 1 --branch tamago-go1.27.4 https://github.com/usbarmory/tamago-go ~/tamago-go
(cd ~/tamago-go/src && ./make.bash)

git clone https://github.com/darianmavgo/godeep.git ~/godeep
cd ~/godeep && make test && make image BOARD=applem1
```

Ubuntu's QEMU can be older than Homebrew's. `check-applem1` needs a QEMU with `sbsa-ref`. If it fails on an old version, build QEMU from source or use a newer Ubuntu release.

**First check:** `make check-applem1` passes in WSL. If it does, the PC builds and rehearses the Mac image exactly like the Mac does.

**Byte-for-byte check (optional):** build `bin/godeep-applem1.img` from the same commit on both machines and compare `sha256sum`. Go builds are reproducible, so they should match. If they don't, a toolchain or `third_party/tamago` difference is the first suspect.

### 3.3 Claude Code and the editor

- Install Claude Code **inside WSL**, so its tools see the Linux paths, `make` and `/dev/ttyACM*`.
- VS Code on Windows with the **WSL** extension: run `code .` from `~/godeep` in WSL.

### 3.4 m1n1's proxyclient

```sh
git clone --recursive https://github.com/AsahiLinux/m1n1 ~/m1n1
python3 -m venv ~/m1n1-venv
~/m1n1-venv/bin/pip install pyserial construct   # see m1n1's README for the current list
echo 'export M1N1DEVICE=/dev/ttyACM0' >> ~/.bashrc
```

`M1N1DEVICE` tells every proxyclient tool which serial port to use. Building m1n1 itself (needed for the hypervisor, §5 loop 3) needs an aarch64 cross toolchain: `sudo apt install gcc-aarch64-linux-gnu`, then `make ARCH=aarch64-linux-gnu-` in `~/m1n1` (check m1n1's README for the current command).

### 3.5 USB passthrough into WSL: usbipd-win

WSL2 is a VM, so by default it doesn't see USB devices. **usbipd-win** forwards a device into it.

In an **administrator** PowerShell:

```powershell
winget install usbipd
```

Then, with the Mac in m1n1 proxy mode and the cable plugged in (§4):

```powershell
usbipd list                         # find the m1n1 device (VID:PID 1209:316d)
usbipd bind --busid <BUSID>         # once per device; needs admin
usbipd attach --wsl --busid <BUSID> --auto-attach
```

**`--auto-attach` matters.** Every time the Mac reboots, m1n1's USB device disconnects and comes back. Without this flag you would have to re-attach it after every run. Leave that PowerShell window open; it re-attaches by itself.

In WSL, `ls /dev/ttyACM*` should now show `ttyACM0` and `ttyACM1`. If you get permission errors, run `sudo usermod -aG dialout $USER` and open a new shell.

---

## 4. One-time setup on the Mac

### 4.1 An entry that boots into m1n1's proxy

You need a boot entry where m1n1 **doesn't** jump to a payload and instead waits for commands over USB. There are two ways to get one:

**A. The Asahi installer's tethered entry.** Run `curl https://alx.sh | sh` again, as in `docs/PartitioningMac.md` §6.5. Choose **"Tethered boot (m1n1, for development)"**; you may need to answer yes to expert mode for it to appear. Name it `godeep-dev`. It takes a few GB of the free space. After recoveryOS approves it, the picker shows **Macintosh HD, Omarchy, godeep, godeep-dev**.

**B. Reuse the godeep entry (to add, no repartitioning).** m1n1 stage 2 drops into proxy mode when it finds **no payload** after itself. So a `boot.bin` made of only `m1n1 | dtbs`, with no kernel, makes the existing godeep entry a proxy entry. `tools/godeep-partition.sh proxy` would install that, keeping the godeep `boot.bin` as `boot.bin.godeep`, and `install` would put it back. This is a small change to `tools/m1n1boot` (§9).

A is ready today. B saves space and keeps one entry, but you switch between "standalone godeep" and "tethered" by reinstalling from macOS.

### 4.2 Make it the default

The point is that **any reboot of the Mac lands in the proxy**, so the PC can push the next run without you touching the Mac. In the startup picker (hold power at boot), select `godeep-dev` (or `godeep` with option B), hold **Option**, and click **Always Use**. To boot macOS or Omarchy, use the picker. To get the old default back, do the same with Macintosh HD.

### 4.3 The cable and the port

- Use a **USB-C data cable**: a Thunderbolt cable, or the USB-C to USB-C cable from a phone or SSD. Many charging cables carry no data. A USB-A to C cable on the PC side works too.
- m1n1 doesn't expose its USB device on every port the same way. Try the **left rear** Thunderbolt port first; if `usbipd list` shows nothing, try the others and note which one works (add it to this doc).
- Keep MagSafe (or another port) for power so the Mac doesn't drain during long sessions.

When m1n1 is waiting, the Mac's screen shows m1n1's logo and log text. On the PC, `usbipd list` shows the device.

**First check (no godeep involved):** with the device attached in WSL,

```sh
~/m1n1-venv/bin/python ~/m1n1/proxyclient/tools/shell.py
>>> p.read32(<some MMIO register>)        # or just: print(u.adt)  # m1n1's view of the hardware
```

If you get a Python prompt that talks to the Mac, the link works.

---

## 5. The dev ladder: four loops, fastest first

Use the cheapest loop that can show the bug. These are the rungs of TargetMyMac.md §7.2, mapped to the two machines.

### Loop 1: host tests (PC only, seconds)

```sh
make test
go test -race -count=50 ./drivers/apple/...
```

Driver logic runs against `mmio.Fake`. When a trace from the hypervisor (loop 3) shows how the real device behaves, copy that sequence into a test fixture so this loop catches the next regression.

### Loop 2: QEMU (PC only, about a minute)

```sh
make check-applem1          # the Mac image on sbsa-ref via fakem1n1, headless
make qemu-applem1           # interactive
make qemu ACCEL=tcg         # board/virt; no HVF on Windows
make check-virt ACCEL=tcg   # PCIe, NVMe, FAT/ext4, USB (check the Makefile passes ACCEL through)
```

This catches boot-path regressions (relocation, page tables, the fault screen) before you touch hardware.

### Loop 3a: chainload on the real Mac (native speed, under a minute)

m1n1's `linux.py` loads a kernel and a device tree, applies m1n1's own fixups to the tree (memory, framebuffer, `/chosen`), and jumps to the kernel with `x0` = the tree. That's the same Linux Image protocol godeep already follows for m1n1 and U-Boot (TargetMyMac.md §3.1), so godeep needs no changes.

```sh
make image BOARD=applem1
~/m1n1-venv/bin/python ~/m1n1/proxyclient/tools/linux.py \
    bin/godeep-applem1.img  bin/t6000-j316s.dtb
```

- **The `.dtb`** is the same tree that `boot.bin` carries. Get it from Asahi's kernel build, or extract it from the godeep ESP's `boot.bin` (`tools/m1n1boot -extract-dtb`, to add, §9). J316s is the 16" M1 Pro. Check `lsdev`'s model line if unsure.
- **What you see:** godeep's console on the Mac's screen, exactly as with a standalone boot. The PC gets no text in this mode: the Mac's UART goes to its debug pins, not to USB.
- **Next run:** type `reboot` in godeep's shell (watchdog reset), or wait for the watchdog if godeep is stuck. The Mac comes back in proxy mode (§4.2), and you run the command again.

Use this loop when you need **real timing**: the SPI keyboard poll, idle, anything the hypervisor would slow down.

### Loop 3b: under m1n1's hypervisor (slower, but you see everything)

This is the loop for writing new Apple drivers. m1n1 runs at EL2 and godeep runs as its guest at EL1. m1n1 can:

- **log every MMIO access** to the devices you choose (address, value, read or write, PC),
- **forward the guest's UART** to the PC's second serial port (`/dev/ttyACM1`),
- **catch exceptions** and give you a Python shell with the guest's registers, instead of a red screen and a photo,
- let you press **Ctrl-C** at any time to stop the guest, inspect memory and continue.

The guest is a regular m1n1 build with godeep appended, the same layout `tools/m1n1boot` already writes into `boot.bin` (`m1n1 | dtbs | gzip(kernel)`):

```sh
# Terminal 1: the guest's UART
picocom -b 115200 /dev/ttyACM1          # sudo apt install picocom

# Terminal 2: run godeep under the hypervisor
make image BOARD=applem1
gzip -kf bin/godeep-applem1.img
cat ~/m1n1/build/m1n1.macho bin/t6000-j316s.dtb bin/godeep-applem1.img.gz > bin/godeep-guest.macho
~/m1n1-venv/bin/python ~/m1n1/proxyclient/tools/run_guest.py bin/godeep-guest.macho
```

Which devices get traced is set by hypervisor scripts (`-m script.py`; see `proxyclient/hv/trace_*.py` for examples). For a new driver, trace just that device's MMIO range. A full trace slows the guest a lot and floods the log.

**Things the hypervisor changes**, so don't trust its timing:

- Every traced access traps to m1n1 and goes over USB. Polled drivers (spihid's 5 ms poll) will see "slow hardware".
- godeep enters at **EL1**, not EL2. `board/applem1`'s entry code already handles both (it drops from EL2 when it finds itself there). Check the `board:` line, which should say EL1.
- The **watchdog** is real hardware. If the hypervisor holds the guest stopped for 30 s while you inspect it, the Mac reboots. Stop the watchdog from the HV shell before long inspections.

**Saving traces as tests.** Write the HV log to a file (`run_guest.py -l trace.log`, or a `tee`), cut the relevant sequence, and turn it into an `mmio.Fake` fixture next to the driver. That's the "rung 2 replays real traces" step of TargetMyMac.md §7.2 and §7.5.

### Loop 4: a standalone install (milestones only)

When a feature works under loops 3a and 3b, boot the Mac into macOS and run `make native` as before. It tests the real boot path (m1n1 → godeep from the ESP, no PC attached). Then switch the default startup disk back to the proxy entry.

---

## 6. Getting output back from the Mac

In order of how soon you can have it:

| Method | Works in | Cost | Gives you |
|---|---|---|---|
| **A webcam pointed at the screen** | every loop | a USB webcam or a phone (as a webcam) and OBS on Windows | live view and recording of the framebuffer, fault screens included, without photos. Today |
| **Samsung UART → hypervisor → `/dev/ttyACM1`** | loop 3b | `drivers/apple/uart` (to add, ~100 lines) | the whole console as text on the PC, copy-pasteable into Claude |
| **The hypervisor's exception handler** | loop 3b | nothing | registers and a Python shell at the fault, instead of the red screen |
| **USB gadget Ethernet** (DWC3 device mode + CDC-ECM, milestone M4) | every loop | a large driver | an IP over the cable: logs, `pprof`, 9P files, SSH. TargetMyMac.md §7.3 |

**Write the UART driver first.** `board/applem1` already tees the console to a PL011 when the tree has one (`early.go`'s `uartBase`, used by fakem1n1). The Mac's tree has a Samsung-style UART instead (`compatible = "apple,s5l-uart"`; its `uart0` node). Its transmit path is two registers: wait until `UTRSTAT` (offset `0x10`) reports the transmitter empty (bit 2), then write the byte to `UTXH` (offset `0x20`). Check these against Linux `samsung_tty.c` or m1n1's `uart.c`. Add it as `drivers/apple/uart` with an `mmio.Fake` test, and have `early.go` pick whichever UART the tree has. Under the hypervisor, m1n1 forwards those writes to `/dev/ttyACM1`. Without a hypervisor they go to pins only Apple's debug tooling can reach, so in loop 3a the screen stays the output.

Once the UART works, `tools/qemucheck`-style scripts can drive the real Mac too: send `help` on `/dev/ttyACM1`, expect the shell's answer. That's `check-applem1` against hardware (§9).

---

## 7. When godeep hangs: getting back to m1n1

| Situation | What happens | What you do |
|---|---|---|
| godeep runs, shell works | — | type `reboot` |
| godeep stops scheduling (no idle, deadlock) | the watchdog reboots the Mac after 30 s | nothing: it comes back in proxy mode |
| Red fault screen | the watchdog is **stopped** on purpose so the screen stays (`lateFault`) | read it (or watch the webcam), then hold power |
| Under the hypervisor | m1n1 catches it and gives you a shell | inspect, then `p.reboot()` from the shell |
| Nothing on screen, no USB device | the boot entry or the cable | hold power 10 s, then check the picker and `usbipd list` |

**A reboot from the PC (to add):** a fault screen is the one case that needs a hand on the Mac. If the fault screen re-armed a short watchdog after drawing, say 2 minutes, the Mac would always come back to the proxy by itself. Make it a boot argument (`godeep.faultreboot=120`) so a standalone boot keeps the screen up forever.

---

## 8. Keeping the code in sync

- **The repo lives on the PC** (`~/godeep` in WSL) during a tethered session. GitHub (`origin`) is the hand-off point.
- Before switching machines, commit or stash and push, then pull on the other side. Treat each machine's working tree as a separate clone.
- The Mac only needs the repo for loop 4 (`make native`), for work that needs HVF (`make qemu` under HVF, the WFI bug), and for `tools/godeep-partition.sh`.
- `bin/` isn't committed; each machine builds its own. The images should be byte-identical from the same commit (§3.2).
- **Claude's memory** is per machine and per path (`~/.claude/projects/...`), so notes saved on the Mac don't appear on the PC. Durable facts belong in `CLAUDE.md` or `docs/`, which travel with git.

---

## 9. Repo changes that make this smooth (to add)

Ordered by how much each one shortens the loop:

| Change | Where | Why |
|---|---|---|
| Samsung UART driver, chosen by `early.go` when the tree has one | `drivers/apple/uart`, `board/applem1` | console as text on the PC under the hypervisor (§6) |
| `make m1-load` = `image BOARD=applem1` + `linux.py` | Makefile | loop 3a in one command |
| `make m1-guest` = image + guest `.macho` + `run_guest.py` (`TRACE=script.py`) | Makefile | loop 3b in one command; already proposed in TargetMyMac.md §7.4 |
| `tools/m1n1boot -extract-dtb` | `tools/m1n1boot` | get `t6000-j316s.dtb` from the `boot.bin` you already have |
| `tools/godeep-partition.sh proxy` (payload-less `boot.bin`) | `tools/m1n1boot`, the script | a proxy entry without repartitioning (§4.1 B) |
| Default `ACCEL` to `tcg` when the host isn't macOS | Makefile (`uname -s`) | `make qemu` and `check-virt` work in WSL without flags |
| Fault screen re-arms the watchdog when `godeep.faultreboot` is set | `board/applem1` | hands-off recovery from faults (§7) |
| `tools/qemucheck` steps over a serial device | `tools/qemucheck` | the same `send:`/`wait:` checks against the real Mac on `/dev/ttyACM1` |

---

## 10. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `usbipd list` shows no m1n1 device | Mac not in proxy mode, a charge-only cable, or the wrong port | Check the Mac's screen shows m1n1 waiting; try another cable, then another port |
| Device listed but no `/dev/ttyACM*` in WSL | not attached | `usbipd attach --wsl --busid <BUSID> --auto-attach` (admin PowerShell) |
| Works once, then gone after the Mac reboots | attached without `--auto-attach` | re-run with `--auto-attach` and leave the window open |
| `Permission denied` on `/dev/ttyACM0` | user not in `dialout` | `sudo usermod -aG dialout $USER`, new shell |
| Proxy tools time out | `M1N1DEVICE` points at `ttyACM1` | it must be `ttyACM0` (commands); `ttyACM1` is the guest UART |
| `make` is very slow in WSL | repo under `/mnt/c` | clone into `~/godeep` |
| `check-applem1` fails only in WSL | old QEMU without a working `sbsa-ref` | newer QEMU (build from source or newer Ubuntu) |
| godeep under the HV boots to an early fault that a standalone boot doesn't | it entered at EL1, or the HV's tree differs | check the `board:` line and the stage squares; compare the trees (`fdt` dump in `lsdev`) |
| Mac reboots while you inspect in the HV shell | the watchdog | stop it from the shell before long pauses |
| The Mac boots macOS instead of the proxy | default startup disk changed (e.g. by a macOS update) | redo §4.2 |

---

## 11. Cheat sheet

```text
PC, admin PowerShell, once per session:
  usbipd attach --wsl --busid <BUSID> --auto-attach

WSL, ~/godeep:
  make test                                   # loop 1
  make check-applem1                          # loop 2
  make image BOARD=applem1                    # build for the Mac
  python ~/m1n1/proxyclient/tools/linux.py bin/godeep-applem1.img bin/t6000-j316s.dtb   # loop 3a
  picocom -b 115200 /dev/ttyACM1              # loop 3b: guest UART (other terminal)
  python ~/m1n1/proxyclient/tools/run_guest.py bin/godeep-guest.macho                   # loop 3b

Mac:
  default startup disk = the proxy entry (hold power → select → Option → Always Use)
  power button held at boot → picker (macOS, Omarchy, standalone godeep)
  in macOS: make native                       # loop 4, milestones only
```
