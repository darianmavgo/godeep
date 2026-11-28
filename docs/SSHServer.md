# An SSH server on godeep, and developing through it

This document lists the steps to run an SSH server on godeep with `golang.org/x/crypto/ssh`, first on QEMU and then on the MacBook Pro, and how development changes once you can `ssh godeep`.

It builds on `docs/ReUsingGVisor.md` §2 (the netstack) and `docs/Using2Computers.md` (the PC as dev host, the Mac as target).

## Status (2026-11-26): steps 1–4 built and running on QEMU

```sh
make qemu          # boots with virtio-net + virtio-rng; sshd listens on the guest's port 22
make ssh           # (another terminal) ssh -p 2222 root@127.0.0.1, with the dev key from make keys
make ssh CMD=ps    # one command
make hosted        # the hosted build serves the same sshd on 127.0.0.1:2222 (host sockets)
make check-virt    # headless: ... rng seeds crypto/rand, ifconfig, ssh ps / lsdev / a pty session
```

| Piece | Where | Notes |
|---|---|---|
| virtio-mmio transport + split virtqueue | `drivers/virtio/mmio` (`transport.go`, `queue.go`), `virtiotest` model | modern (version 2) only: QEMU needs `-global virtio-mmio.force-legacy=false` |
| virtio-rng | `drivers/virtio/rng` | seeds `board/virt`'s DRBG (`mixSeed`) at start; `lsdev rng0` says `seeded crypto/rand` |
| virtio-net | `drivers/virtio/net` | publishes a `netdev.NIC`; interrupts under TCG, polled under HVF (2 ms busy, 20 ms idle; idle cost measured ~2% of a host core, unchanged) |
| TCP/IP | `net/netstack` (gVisor) | `gonet` listeners and conns; no `net.SocketFunc` global |
| network capability | `kernel/cap` (`Net`, `PendingNet`), `kernel/net.go` (`netd`), `net/hostnet` | `netd` follows the NIC across driver restarts; hosted uses host sockets |
| sshd | `programs/sshd`, keys from `make keys` | pty sessions on `tty.NewFor` + `sh`, `exec` (`sh -c`), exit status, ^C, kill-on-disconnect, stdin EOF doesn't kill `exec`; `FuzzPreAuth` |
| checks | `tools/qemucheck` `local:` steps | `check-virt` passes under HVF (polled) and TCG (irq); `check-usbnet` exercises SSH over USB CDC-ECM (`usb-net`) |

Since then (2026-11-26): **sftp/scp** (`programs/sshd/sftp.go`, onto writable volumes like `ram:` and writable FAT), **WASM programs** pushed with scp and run without a reboot (`programs/wasm`, `make push`), **DHCP (with lease renewal) + DNS + HTTPS** (embedded Mozilla TLS roots via `x509roots/fallback`), **9P** (`drivers/virtio/p9`, `fs/ninep` read/write, and `tools/ninepd` TCP 9P2000.L host server), **USB CDC-ECM** (`drivers/usb/xhci/ecm.go`, `make check-usbnet`), the **kernel log** (`dmesg`, `ssh godeep dmesg`), and **profiles** (`ssh godeep pprof heap > heap.pb.gz`), all tested in `check-virt`.

Not yet: `window-change` handling beyond acknowledging it, and the Mac transport layers (steps 5 and 6 below). The sections below are the original plan; where the code differs, the code is right.

## Contents

- [TL;DR](#tldr)
- [1. What has to exist](#1-what-has-to-exist)
- [2. Step 1: a link that carries bytes](#2-step-1-a-link-that-carries-bytes)
- [3. Step 2: entropy](#3-step-2-entropy)
- [4. Step 3: keys](#4-step-3-keys)
- [5. Step 4: `programs/sshd`](#5-step-4-programssshd)
- [6. Step 5: sessions run the existing shell](#6-step-5-sessions-run-the-existing-shell)
- [7. Tests](#7-tests)
- [8. Developing over SSH afterwards](#8-developing-over-ssh-afterwards)
- [9. What SSH won't replace](#9-what-ssh-wont-replace)
- [10. Suggested order](#10-suggested-order)
- [11. Risks and open questions](#11-risks-and-open-questions)
- [12. Parallels: ssh from another computer on the LAN](#12-parallels-ssh-from-another-computer-on-the-lan)

---

## TL;DR

- `x/crypto/ssh` is pure Go and runs on TamaGo unchanged. The work is everything around it: a link, real entropy, keys, and connecting a session to godeep's shell.
- **The shell needs almost no changes.** `sh.Env` carries a `*tty.TTY`, and `tty.New(in <-chan byte, out io.Writer, echo bool)` takes any byte channel. An SSH session channel *is* a byte stream, so each session gets its own `tty.TTY` and its own `sh`, the same way the console does.
- **SSH doesn't need TCP/IP.** `ssh.NewServerConn` takes a `net.Conn`, and the OpenSSH client can run over any byte pipe (`ProxyCommand`). So there are two ways to reach godeep:
  - **IP:** virtio-net + gVisor netstack on QEMU, and later DWC3 gadget Ethernet (CDC-ECM) over the Mac's USB-C cable (milestone M4).
  - **A serial line:** SSH over a UART, through `ProxyCommand`. On the Mac this works under m1n1's hypervisor, which forwards the guest UART to the PC as `/dev/ttyACM1`. That's SSH on real hardware **before** the DWC3 driver exists.
- Order: netstack on QEMU → virtio-rng → `programs/sshd` → pty and kill-on-disconnect → the Mac (serial under the hypervisor, then USB Ethernet).

---

## 1. What has to exist

```
 PC: ssh godeep
        │
        ▼
 ┌──────────────── link ────────────────┐
 │ IP:     QEMU user-net hostfwd :2222  │   or   serial: ProxyCommand → UART
 │         Mac: USB-C CDC-ECM (M4)      │          (QEMU second UART, Mac under m1n1 HV)
 └──────────────────────────────────────┘
        │ net.Conn
        ▼
 programs/sshd  (a Proc; holds a listener capability, host key, authorized keys)
        │  ssh.NewServerConn: kex (needs crypto/rand), auth (public key)
        ▼
 per session: a child Proc
        tty.New(bytes from the channel, the channel as writer, echo=true)
        sh.Main(sh.Env{Table, TTY: that tty, ...})
```

| Piece | Exists today? | Where |
|---|---|---|
| Shell, tty, process table, ^C, kill | yes | `programs/sh`, `kernel/tty`, `kernel/proc` |
| virtio-mmio probing | probe only | `drivers/virtio/mmio` (reports the device ID; no net/rng driver yet) |
| TCP/IP | no | gVisor netstack via `usbarmory/go-net` (`docs/ReUsingGVisor.md` §2) |
| Real entropy | **no** | the RNG is seeded from the boot-time counter (`board/*/rng.go`) |
| A UART with input on the Mac | no | `drivers/apple/uart` (`docs/Using2Computers.md` §6) |
| USB Ethernet on the Mac | no | DWC3 device mode + CDC-ECM (`docs/TargetMyMac.md`, M4) |
| `x/crypto/ssh` | not in `go.mod` | `go get golang.org/x/crypto/ssh` |

---

## 2. Step 1: a link that carries bytes

### 2.1 IP on QEMU (do this first)

This is the well-trodden path: `tamago-example` already runs gVisor's netstack on TamaGo.

1. **A virtio-net driver** for `board/virt`. Either use `github.com/usbarmory/go-net/virtio`, or write `drivers/virtio/net` on top of the existing `drivers/virtio/mmio` probe and `kernel/dma`, following `docs/DriverModel.md`: one goroutine owns the device, RX completions arrive over `<-line.C`, and it publishes a frame service with `d.Ready(svc)`. Probe the slots by `DeviceID` (1 = net) instead of hard-coding the address.
2. **The netstack**: `gnet.NewGVisorStack(...)` from `go-net`, with QEMU's user-network defaults set statically (guest `10.0.2.15/24`, gateway `10.0.2.2`, DNS `10.0.2.3`).
3. **The Go hook**: `net.SocketFunc = stack.Socket` (tamago-go's `net` package). After this, `net.Listen("tcp", ":22")` works.
4. **A capability, not a global.** The hook is global, but godeep's rule is that authority comes from capabilities. Wrap it as a `cap.Net` with `Listen(ctx, network, addr)` (`docs/ReUsingGVisor.md` §2 has a sketch) and give it only to `sshd`.
5. **QEMU flags** (a `NET=1` option in the Makefile):

   ```
   -device virtio-net-device,netdev=net0 -netdev user,id=net0,hostfwd=tcp::2222-:22
   ```

   QEMU's virt machine puts virtio-mmio slots in the device tree, so `board/virt/devices.go` sees the new device.

Check: `ssh -p 2222 localhost` reaches godeep's port 22, even if the handshake fails at first.

### 2.2 IP on the Mac: USB-C gadget Ethernet (milestone M4)

A DWC3 device-mode driver plus CDC-ECM framing turns the USB-C cable into an Ethernet link, and the same netstack and the same `sshd` run on it. The PC sees a USB network adapter (in WSL, attach it with usbipd as in `docs/Using2Computers.md` §3.5, or let Windows own it and route). This is the largest piece of the whole plan (§11).

### 2.3 A serial line: SSH without TCP/IP

`ssh.NewServerConn(c net.Conn, config)` only reads and writes bytes. A UART wrapped as a `net.Conn` (no-op deadlines, fixed `LocalAddr`/`RemoteAddr`) is enough. On the PC, OpenSSH pipes the connection through a command:

```
# ~/.ssh/config on the PC (WSL)
Host godeep-serial
    User root
    ProxyCommand socat - /dev/ttyACM1,raw,echo=0,b115200
    IdentityFile ~/.ssh/godeep_dev
```

One SSH connection multiplexes channels, so a single serial line still carries several shells, `exec` commands and sftp at the same time.

| | IP (netstack) | Serial line |
|---|---|---|
| Needs | NIC driver + gVisor | a UART with RX and TX |
| On the Mac | DWC3 + CDC-ECM (large) | `drivers/apple/uart` under m1n1's hypervisor (small) |
| Speed | USB 2/3 speed | the UART's speed (115200 baud is about 11 KB/s: fine for shells, slow for file copies) |
| Connections | many, plus port forwarding | one SSH connection (many channels inside it) |
| Conflicts | none | the UART can't also be the plain-text console. Pick one at boot (a `godeep.uart=ssh` boot argument) |

The serial path is the bridge: it gives SSH on the real Mac **weeks** earlier, through the hypervisor loop that `docs/Using2Computers.md` already sets up.

---

## 3. Step 2: entropy

SSH's key exchange generates ephemeral keys (curve25519) from `crypto/rand`. godeep's RNG today is seeded from the boot-time counter, so **an attacker who can guess the boot time can recover session keys**. That's acceptable only on a link nobody else can reach, and only until this step is done.

| Target | Source | Notes |
|---|---|---|
| QEMU | **virtio-rng** (`-device virtio-rng-device`, `DeviceID` 4) | a small `drivers/virtio/rng` driver; `board/virt`'s `GetRandomData` reads from it. Already planned in CLAUDE.md |
| Mac | unknown yet | The M1 doesn't seem to implement the ARMv8.5 `RNDR` instruction (check `ID_AA64ISAR0_EL1.RNDR`). The SoC's TRNG is behind the Secure Enclave. Until something better exists: CPU jitter, plus a seed the PC sends over the link at boot (hypervisor shell or `ProxyCommand`) |

Use **ed25519** host and user keys. Ed25519 signatures are deterministic, so a weak RNG doesn't leak the long-term key the way ECDSA would. Only the session keys depend on the RNG.

---

## 4. Step 3: keys

Development keys, built into the image at build time and never committed:

| Key | Where it comes from | How it gets into the image |
|---|---|---|
| Host key (godeep's identity) | `ssh-keygen -t ed25519 -N '' -f bin/godeep_host_ed25519` (Makefile, once) | `//go:embed` from a generated file, or `-ldflags -X` with the PEM as a string |
| Authorized key (who may log in) | the PC's `~/.ssh/godeep_dev.pub` | `make image SSH_AUTHORIZED_KEYS=~/.ssh/godeep_dev.pub` |

- Generating the host key at boot is cleaner, but it needs the real entropy from §3 and somewhere to keep it (GODEEPDATA, once godeep can write FAT32). Until then, a key built into the image is simpler, and stable across reboots, so `known_hosts` doesn't complain every time.
- **Public-key auth only.** No passwords: there's nothing to check them against, and the link is for development.
- `bin/` is already outside git. Keep the generated keys there.

---

## 5. Step 4: `programs/sshd`

A program registered from `init()` like the others. A sketch (shapes, not final code):

```go
// programs/sshd/sshd.go
func init() {
	prog.Register(prog.Program{Name: "sshd", Usage: "sshd [ADDR]", Main: Main})
}

func Main(ctx context.Context, p *proc.Proc) error {
	cfg := &ssh.ServerConfig{PublicKeyCallback: checkAuthorized}
	cfg.AddHostKey(hostKey)

	ln, err := netCap.Listen(ctx, "tcp", ":22") // a capability given to this Proc
	if err != nil {
		return err
	}
	p.Go(func() { <-ctx.Done(); ln.Close() }) // kill → Accept returns

	for {
		c, err := ln.Accept()
		if err != nil {
			return ctx.Err()
		}
		p.Go(func() { serveConn(ctx, p, c, cfg) })
	}
}

func serveConn(ctx context.Context, p *proc.Proc, c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	p.Go(func() { ssh.DiscardRequests(reqs) })
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		p.Go(func() { session(ctx, p, ch, creqs) }) // §6
	}
}
```

Points that matter for godeep's rules:

- **Kill must work.** Closing the listener and the connections when `ctx` is cancelled is what makes `kill <sshd pid>` return promptly. Every blocking call above is either on `ctx` or on something closed when `ctx` ends.
- **Bare `go` statements inside libraries.** `x/crypto/ssh` (and gVisor) start their own goroutines with `go`. They inherit the pprof labels of the goroutine that started them, so `ps` still attributes them to `sshd`'s PID. But they aren't counted by `p.Go`, and **a panic in one crashes the kernel**, not just the Proc. Accept this for trusted libraries, and note it next to the rule in CLAUDE.md. Fuzz the handshake with host tests (§7) so a malformed packet can't reach such a panic.
- **Start it from `initd`** on boards that have a link, or by hand from the console (`sshd &`, if the shell gets background jobs; otherwise `initd` spawns it).
- **Image size.** `x/crypto/ssh` adds roughly 1–2 MB, gVisor more. Measure `bin/godeep-applem1.img` before and after.

---

## 6. Step 5: sessions run the existing shell

Each SSH session becomes a child Proc running `sh`, with its own `tty.TTY` over the channel:

```go
func session(ctx context.Context, p *proc.Proc, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	in := make(chan byte, 256)
	// … reqs: accept "pty-req", "shell", "exec", "window-change"; reply to each …
	p.Go(func() { pump(ctx, ch, in) }) // ch.Read → in; closes in at EOF
	t := tty.New(in, crlf{ch}, true)   // echo: the client's terminal is raw
	defer t.Close()
	env := shEnv                       // sshd's copy of sh.Env, with this TTY
	env.TTY = t
	child := env.Table.Spawn(ctx, p.PID, proc.Spec{Name: "sh", Stdout: t, Main: sh.Main(env)})
	select {
	case <-child.Done():
	case <-ctx.Done():
	}
	child.Kill() // the client hung up, or sshd was killed
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exitCode(child)}))
}
```

| Concern | What to do |
|---|---|
| **pty** (`pty-req`) | Accept it. godeep's `tty` already does echo, line editing and ^C, which is everything a pty would. Echo on, because the client puts its terminal in raw mode. Ignore the window size for now (`window-change`), or store it for future full-screen programs |
| **Newlines** | A raw terminal needs `\r\n`. Wrap the channel in a writer that turns `\n` into `\r\n` (`crlf` above), the way the serial console would |
| **^C** | Arrives as byte `0x03` in the channel. The tty turns it into `Interrupts()`, so `sh` kills the foreground program, as on the console |
| **^D / logout** | `ReadLine` returns EOF, `sh` exits, the session sends `exit-status` and closes |
| **Disconnect** | The channel read returns EOF → `in` is closed → the tty closes → the shell's `ReadLine` returns EOF. Kill the child too, so a program running in the foreground doesn't outlive its session |
| **`exec` requests** (`ssh godeep ps`) | Run one command: `sh` with the command as its only line (or `prog.Lookup` directly), no echo, then `exit-status`. This is what makes scripting possible (§8) |
| **The tty goroutine** | `tty.New` starts its discipline goroutine as a kernel thread (`proc.Kthread`, pid 0). That's right for the console but leaks one pid-0 goroutine per session if `Close` is ever missed. Let `tty.New` take the owning Proc (or a `go` function) so a session's tty belongs to that session |
| **Authority** | A remote shell gets the same `sh.Env` as the console, i.e. the same authority. That's fine for a dev link. When `cap` exists, sshd can hand each session a narrower set |
| **sftp** | Later: `github.com/pkg/sftp` serves the `sftp` subsystem over any `io.ReadWriter`, backed by an `io/fs.FS` (the read-only `fs/fat` and `fs/ext4` already are). Writes need a writable filesystem first |

---

## 7. Tests

- **Host tests** (`programs/sshd`, run with `go test -race -count=50`):
  - `ssh.NewServerConn` over `net.Pipe`, with an `x/crypto/ssh` client on the other end that runs `echo` and `ps` and checks the output.
  - Auth: an unknown key is rejected, a known one is accepted.
  - Kill-on-disconnect: start a program that blocks, close the client, and check that its Proc ends.
  - Kill `sshd` itself: every goroutine returns, and `Accept` doesn't hang.
  - A fuzz test (`go test -fuzz`) on the bytes before authentication.
- **`tools/qemucheck`**: boot `board/virt` with `hostfwd`, run `ssh -p 2222 localhost ps`, and expect the process table. Add the serial path the same way with a second QEMU serial port and `ProxyCommand`.

---

## 8. Developing over SSH afterwards

### Day-one benefits

- **Shell output as text on the PC: no photos.** Copy it straight into Claude.
- `lsdev`, `ps`, `mem` and `evtest` from a terminal while the Mac's screen shows something else.
- **Script native checks** the way `qemucheck` does: `ssh godeep lsdev hid | grep 'crc ok'`.
- **Claude Code can run them itself.** From the PC, the agent can `ssh godeep lsdev` and read the answer, so checks on real hardware become part of an ordinary edit–test loop instead of a photo you describe.

A host entry for the IP link:

```
# ~/.ssh/config on the PC
Host godeep
    HostName localhost       # QEMU hostfwd; the Mac's USB address later
    Port 2222
    User root
    IdentityFile ~/.ssh/godeep_dev
    StrictHostKeyChecking accept-new
```

### Next steps that build on it

1. ✅ **scp/sftp → loading programs:** push WASM programs (the planned wazero loader) and run them without rebuilding the kernel. That's the real dev-cycle win: only kernel changes need a reboot.
2. ✅ **Logs:** `ssh godeep dmesg` (`dmesg -f` follows).
3. ✅ **pprof** (goroutine, heap…; no CPU profile on TamaGo): serve `net/http/pprof` on the netstack, or stream profiles over an SSH `exec` channel, then run `go tool pprof` on the PC. It works with godeep's existing `pid`/`proc` goroutine labels.
4. ✅ **Files:** 9P over virtio (`drivers/virtio/p9`) and over TCP (`fs/ninep` read/write + `tools/ninepd` host server), so godeep can read and write a directory on the dev host.
5. **Kernel updates without a disk write:** an `sshd` subcommand that receives a new Image and hands it to m1n1 or a kexec-like path. This is further out; until then, use `linux.py` through the m1n1 proxy (`docs/Using2Computers.md` §5).

---

## 9. What SSH won't replace

- **Early-boot and fault debugging.** Nothing runs before the network does. Keep the boot-stage squares, the fault screen and m1n1's hypervisor.
- **Kernel rebuilds.** Use the chainload loop for those (`docs/Using2Computers.md` §5).
- **Timing-sensitive driver work.** An SSH session is extra load on a single core with `GOMAXPROCS=1`. Disconnect when measuring idle or polling behaviour.

---

## 10. Suggested order

1. ✅ **virtio-net + netstack on QEMU** (`board/virt`), `hostfwd` 2222, behind a `cap.Net`.
2. ✅ **virtio-rng**, wired into `board/virt`'s `GetRandomData`.
3. ✅ **`programs/sshd`** with a built-in dev host key and authorized key, plus host tests and a `qemucheck` script.
4. ✅ **pty + tty discipline**, ^C, `exec`, kill-on-disconnect, sessions owning their tty goroutine.
5. **On the Mac, the bridge:** `drivers/apple/uart` with RX, `godeep.uart=ssh`, and SSH over `/dev/ttyACM1` under m1n1's hypervisor (§2.3).
6. **On the Mac, the real link:** DWC3 gadget + CDC-ECM (M4). The same `sshd` then works over the USB-C cable at USB speed, without the hypervisor.
7. ✅ sftp → ✅ WASM program loading → ✅ pprof → ✅ 9P (virtio-9p + `tools/ninepd` over TCP).

---

## 11. Risks and open questions

- **DWC3 device mode on the M1 Pro** also needs the Type-C PHY (ATC) and possibly the CD3217 port controller to switch roles. That makes it the biggest unknown. The serial bridge (§2.3) is the fallback that keeps SSH usable on the Mac meanwhile.
- **Entropy on the Mac** (§3). Until there's a real source, treat the link as trusted-cable-only and say so in `sshd`'s banner.
- **Panics in library goroutines** take down the kernel (§5). Fuzzing helps; containing them would need changes to the libraries or to how godeep recovers.
- **`net.SocketFunc` is tamago-go specific.** Host builds (`board/hosted`) use the real OS network, so `sshd` should only depend on `cap.Net` and run on both. That also gives a fast host loop: `make hosted`, then `ssh -p 2222 localhost`.
- **The hypervisor UART's speed and buffering** for SSH under m1n1 are untested. If it's too slow or loses bytes, the bridge won't hold and USB Ethernet becomes the only path.
- **Image size and boot time** with gVisor + SSH on the Mac: measure them, since every chainload transfers the whole image.

---

## 12. Parallels: ssh from another computer on the LAN

godeep on Parallels Desktop (`docs/Parallels.md`) sits on Parallels' **Shared** network by default: the VM gets `10.211.55.x` from Parallels' DHCP, behind NAT. The Mac itself reaches that address directly (`make ssh-parallels`, tested). Other computers on the LAN can't, so pick one of the two ways below. Either one puts a root shell on your LAN, so read the security notes first.

Status (2026-11-27): the Mac-side path (`make ssh-parallels`, `scp` into `ram:`) is tested. The two LAN setups below are written from Parallels' CLI and haven't been run yet: they open the VM to the LAN, so you run them.

### 12.1 Security first

- **Logins are by key only** (no passwords), against the `authorized_keys` built into the image (`make keys`; `programs/sshd/keys/`). Anyone who has `bin/ssh/id_ed25519` gets a root shell, and in godeep that shell has the whole machine (volumes, network, `halt`).
- **The host key is the development key** `bin/ssh/host_ed25519`, the same on every build: fine for telling *your* godeep apart, not a secret.
- **No real entropy on this board** (sshd's banner says so): `board/virt` on Parallels has no virtio-rng, so `crypto/rand` is seeded from the counter. Keys exchanged in a session are predictable in principle. Use it on a LAN you trust, never on a network you share with strangers, and remove the forward or bridge when you're done.

### 12.2 Authorize the other computer

Better than copying the Mac's dev private key around: build the other computer's public key into the image.

```sh
# on the other computer (create a key once if it has none: ssh-keygen -t ed25519)
cat ~/.ssh/id_ed25519.pub            # copy this line to the Mac, e.g. into ~/laptop.pub
# on the Mac
make parallels SSH_AUTHORIZED_KEYS=~/laptop.pub   # rebuilds the image with the key, reboots the VM
```

`SSH_AUTHORIZED_KEYS` takes one or more files (space separated); they're appended to the dev key. The keys are part of the image, so pass it on every build that should keep them (or set it in your environment).

### 12.3 Option A: forward a port on the Mac (keeps the Shared network)

Parallels' NAT can forward a port of the Mac to the VM. The rule names the VM, so it keeps working when the VM's DHCP address changes.

```sh
# on the Mac: forward the Mac's TCP 2224 to GoDeep's port 22
prlsrvctl net set Shared --nat-tcp-add godeep-ssh,2224,GoDeep,22
prlsrvctl net info Shared            # lists the rule under "NAT server"
ipconfig getifaddr en0               # the Mac's LAN address, e.g. 192.168.4.36

# on the other computer
ssh -p 2224 root@192.168.4.36

# when you're done
prlsrvctl net set Shared --nat-tcp-del godeep-ssh
```

- If `prlsrvctl` doesn't accept the VM's name in the rule, use its UUID (`prlctl list -i GoDeep | grep '^ID'`).
- Use a port other than 2222: `make qemu`/`make hosted` listen on 127.0.0.1:2222, and check-virt on 2223.
- If the macOS firewall is on (System Settings → Network → Firewall), allow incoming connections for Parallels' NAT service when macOS asks, or under the firewall's Options.
- It only works while the Mac is awake and the VM is running.

### 12.4 Option B: bridge the VM onto the LAN

The VM gets its own address from the LAN's router, as if it were another computer on the network.

```sh
# on the Mac
prlctl stop GoDeep --kill
prlctl set GoDeep --device-set net0 --type bridged --iface en0   # en0: the Mac's Wi-Fi (or its Ethernet)
make parallels
make ssh-parallels CMD=ifconfig      # won't work here: it reads Parallels' Shared-network leases
```

Read the address on the VM's window or serial console (`nc -U bin/parallels/serial.sock`, then `ifconfig`): for example `eth0: 192.168.4.57/24 (dhcp from 192.168.4.1)`. Then, from the other computer:

```sh
ssh root@192.168.4.57
```

- Bridging over Wi-Fi works on most home networks. Networks with client isolation or one-device-per-login (offices, hotels) may not give the VM an address, or may block traffic between clients: use option A there.
- The address can change on every boot (the router's DHCP). Reserve one for the VM's MAC (`prlctl list -i GoDeep | grep net0`) in the router if you use it often.
- Go back to the Shared network with `prlctl set GoDeep --device-set net0 --type shared`.

### 12.5 On the other computer

```
# ~/.ssh/config
Host godeep-pd
    HostName 192.168.4.36    # option A: the Mac's address; option B: the VM's
    Port 2224                # option A only (B: 22)
    User root
    IdentityFile ~/.ssh/id_ed25519
```

```sh
ssh godeep-pd                         # a shell (pty session)
ssh godeep-pd 'lsdev cpus; top -n 1'  # one command
scp notes.txt godeep-pd:              # lands in ram:/notes.txt (RAM, lost at reboot)
ssh godeep-pd 'curl -s -w "%{http_code}\n" -o ram:/x https://example.com'
```

The first connection asks to trust godeep's host key: its fingerprint is in the VM's `dmesg` (`sshd: listening on :22, host key SHA256:...`), the same value on every build made with this Mac's `bin/ssh`.

