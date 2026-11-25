# Reusing gVisor code in godeep: which "syscalls" are worth building

[gVisor](https://gvisor.dev) is Google's application kernel: a Linux-compatible kernel written in Go. godeep is also a kernel written in Go. This doc answers two questions:

1. **Which parts of gVisor can godeep reuse:** import directly, port, use as a design reference, or skip?
2. **Which system-call families are worth implementing,** including networking (Ethernet/TCP), file storage and the other OS basics, and in what order?

This was checked against godeep's real environment: TamaGo v1.27.4, tamago-go 1.27.4, QEMU 11.1 on macOS, and `tamago-example`'s working network code.

> **Board note:** godeep now boots **arm64 on QEMU `virt` (HVF)** by default. On `virt`, virtio-mmio devices sit at `0x0a000000` in 32 slots, `0x200` apart, with interrupts on GIC SPIs starting at INTID 48. The microvm addresses below (`0xfeb00000`, `VIRTIO_NET0_BASE`) apply to `ARCH=amd64`. Either way, godeep's N1 step (probe slots by `DeviceID`) hides the difference, and `go-net`'s virtio driver only needs the base address and IRQ.

---

## TL;DR

- **Reuse gVisor's network stack (`pkg/tcpip`) directly.** It's already proven on TamaGo + QEMU microvm through `github.com/usbarmory/go-net`. Once it's wired in, Go's normal `net`, `net/http`, `crypto/tls` and SSH libraries work in godeep unchanged. **Highest value, lowest effort.**
- **Don't import gVisor's kernel ("the Sentry").** Its filesystems, syscall table, memory manager and process model assume a Linux host underneath (memfd, mmap, host file descriptors, ptrace/KVM platforms) and are tied together by `kernel.Task`. Use its **VFS, kernfs, tmpfs and erofs as design references** for godeep's own filesystem layer.
- **Don't implement the Linux syscall ABI.** godeep is Go-only (one address space), so a "syscall" is a method on a capability. About 60% of Linux syscalls (mmap, futex, epoll, signals, ptrace, setuid…) solve problems godeep doesn't have.
- **Build order:**
  1. **Networking** (go-net + gVisor)
  2. **VFS + procfs/devfs/memfs**
  3. **9P host folder sharing** (edit on your Mac, use in godeep)
  4. **virtio-blk + FAT32** (persistent disk)
  5. pipes, then the rest

---

## 1. What gVisor is made of

```
┌──────────────────────────────── gVisor ────────────────────────────────┐
│ runsc (OCI runtime, CLI)                                    ✗ skip     │
├────────────────────────────────────────────────────────────────────────┤
│ Sentry = the Go kernel                                                 │
│   syscalls/linux   ~300 Linux syscalls                      ✗ skip     │
│   kernel           Task (one goroutine per app thread), signals,       │
│                    futex, cgroups, IPC                       ◐ reference│
│   vfs              mounts, path walking, FileDescription     ◐ reference│
│   fsimpl/*         tmpfs, proc, sys, dev, kernfs, pipefs,              │
│                    erofs, overlay, gofer(→host files)        ◐ reference│
│   mm, loader, memmap, pgalloc   address spaces, ELF          ✗ skip     │
│   platform/{systrap,ptrace,kvm} trap app syscalls            ✗ skip     │
├────────────────────────────────────────────────────────────────────────┤
│ pkg/tcpip = "netstack"   Ethernet, ARP, IPv4/6, ICMP, TCP,  ✅ REUSE   │
│                          UDP, raw sockets, routing                     │
│ pkg/buffer, pkg/waiter, pkg/sync, pkg/atomicbitops           ✅ (come    │
│                                                           with tcpip)  │
└────────────────────────────────────────────────────────────────────────┘
```

gVisor and godeep share a philosophy. In gVisor's Sentry, **each application thread is a goroutine** (`kernel.Task` runs in its own goroutine), and Go's scheduler multiplexes them. That's godeep's core idea too. The difference is who sits underneath and what the kernel serves:

|  | gVisor Sentry | godeep |
|---|---|---|
| runs on | a Linux host (memfd, mmap, host fds, seccomp) | bare metal (TamaGo) |
| runs | unmodified Linux binaries (ELF, x86/arm syscall ABI) | Go code linked into the kernel |
| isolation | the Sentry traps every app syscall | Go memory safety + capabilities |
| build | Bazel; most of the Sentry is `//go:build linux` | plain `go build`, `GOOS=tamago` |

The last row decides most of this. The Sentry's packages **don't compile for `GOOS=tamago`**, and pulling one in drags in `kernel.Task`, `auth.Credentials` and `pgalloc`'s host-memory file, and with them the rest of the Sentry. `pkg/tcpip` is the exception. It's designed to be embedded: Tailscale, wireguard-go and TamaGo all embed it.

---

## 2. Tier A: reuse directly: gVisor netstack (Ethernet → TCP/IP)

### It's already been done on our exact platform

`tamago-example` runs gVisor's netstack on QEMU microvm today. The pieces:

| Piece | Package | Role |
|---|---|---|
| NIC driver | `github.com/usbarmory/go-net/virtio` (`vnet.Net`) | virtio-net over `tamago/kvm/virtio.MMIO` at `microvm.VIRTIO_NET0_BASE`, IRQ 47 |
| TCP/IP | `gvisor.dev/gvisor/pkg/tcpip` via `gnet.NewGVisorStack(...)` | Ethernet frames ↔ TCP/UDP |
| **Go std-lib hook** | `net.SocketFunc = stack.Socket` | tamago-go's `net` package calls this for every socket, so `net.Listen`, `net.Dial`, `http.ListenAndServe` all work |
| DNS | `net.SetDefaultNS([]string{...})` | the resolver goes through the same stack |
| Alternative stack | `github.com/soypat/lneto` (`STACK=lneto` in tamago-example) | smaller, simpler, fewer features |

`net.SocketFunc` is a hook in **tamago-go's** `net` package (`src/net/net_tamago.go`). One assignment makes gVisor the backend of Go's whole standard networking library. That's the reason this tier is cheap.

### What godeep adds on top

The plain hook is a global, which goes against godeep's capability rule. Wrap it so the network is something a process is *given*:

```go
// kernel/cap/net.go (sketch)
type Net interface {
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
	Listen(ctx context.Context, network, addr string) (net.Listener, error)
}
// netCap enforces a per-process policy (e.g. allowed ports/hosts) and then
// calls net.Dialer / net.ListenConfig. Their sockets are owned by the gVisor
// stack's goroutines and are closed automatically when the process's ctx is
// cancelled.
```

The netstack's own goroutines (TCP timers, packet processing) show up in `ps` as PID 0 kernel threads if they're started under `proc.Kthread` labels. The NIC interrupt handler becomes a goroutine that turns IRQs into channel sends, exactly as the plan describes.

### Running it on your Mac

tamago-example uses a `tap` device, which needs root and Linux-style setup. On macOS, use QEMU's **user-mode NAT** instead:

```sh
-device virtio-net-device,netdev=net0 \
-netdev user,id=net0,hostfwd=tcp::8080-:80,hostfwd=tcp::2222-:22
```

| Setting | Value |
|---|---|
| guest IP / gateway / DNS | `10.0.2.15/24`, `10.0.2.2`, `10.0.2.3` (QEMU defaults; godeep configures them statically, no DHCP needed) |
| Mac → godeep web server | `http://localhost:8080` reaches godeep's port 80 |
| Mac → godeep SSH | `localhost:2222` reaches godeep's port 22 |

`vmnet-shared` gives the guest a real LAN address but needs root.

### Cost / risk

| Item | Assessment |
|---|---|
| Effort | **days**: mostly copying tamago-example's `network/` wiring and adapting addresses |
| Image size | grows noticeably (gVisor plus TLS); measure after wiring |
| CPU under TCG | fine for SSH/HTTP; slow for bulk transfers |
| Device address | tamago hard-codes the net device at virtio-mmio slot `+0x2e00`. Once godeep adds more virtio devices (blk, 9p, rng), it should **probe** the slots (read `DeviceID`: 1 = net, 2 = blk, 4 = rng, 9 = 9p, 16 = gpu) instead. |
| License | gVisor is Apache-2.0. Importing it is fine; if you copy or port code, keep its license headers and NOTICE. |

**Verdict: do it first.** It unlocks remote shells (SSH into godeep), an HTTP server, NTP time, and the GPU-offload option in `GoGpuSupport.md`.

---

## 3. Tier B: port the design, not the code: gVisor's filesystem layer

gVisor has the best-engineered Go VFS in existence. You can't import it into godeep, but you should copy its *shape*:

| gVisor component | What to take | godeep version |
|---|---|---|
| `sentry/vfs` | the mount table; path walking that crosses mounts; `FileDescription` (an open file) kept separate from `Dentry` (a name) | `kernel/vfs`: `Mount`, `Walk(ctx, path)`, `File` |
| `sentry/fsimpl/kernfs` | a framework for **synthetic** filesystems: directories whose entries are generated by Go functions | base for `procfs` and `devfs` |
| `sentry/fsimpl/proc` | the layout of `/proc/<pid>/{status,stat,task/…}` | `/proc/<pid>/status` from `proc.Table` + `proc.Snapshot()`, `/proc/<pid>/goroutines` (godeep-specific: `ps -g` as a file) |
| `sentry/fsimpl/tmpfs` | in-memory inodes with correct link counts, rename and timestamps | `memfs` |
| `sentry/fsimpl/erofs` | a **read-only image format** that loads fast | optional: ship a root filesystem image inside the kernel ELF |
| `sentry/fsimpl/gofer` + `lisafs` | how a sandboxed kernel talks to a host file server | godeep uses **9P** for this instead (see §5), because QEMU on macOS supports virtio-9p |
| `sentry/fsimpl/overlay` | copy-up union mounts | later: a writable layer over a read-only base image |

**godeep's filesystem API should be Go's own `io/fs`.** Every filesystem implements `fs.FS` plus a small write extension (`Create`, `Mkdir`, `Remove`, `Rename`), so programs can use `fs.WalkDir`, `fs.ReadFile`, `http.FileServer(http.FS(...))` and `text/template` unchanged.

Processes receive a **`cap.FS` rooted wherever the parent chooses**. That's chroot for free, with no global `/`.

### A note on TamaGo's built-in filesystem

tamago-go already gives `os.Open`/`os.WriteFile` an **in-memory Unix-like filesystem** (`src/syscall/fs_tamago.go`, adapted from NaCl), and `syscall.MkDev` registers device files like `/dev/null`. It works today, but it's **one global namespace with one global lock**, which is the opposite of the capability model. Use it only as scratch space for third-party libraries that insist on `os`. godeep's own programs should use `cap.FS`.

---

## 4. Tier C: skip: the Linux ABI machinery

| gVisor component | Why godeep skips it |
|---|---|
| `syscalls/linux` (the ~300-entry syscall table) | godeep doesn't run Linux binaries. Revisit only if you reverse the "Go-only" isolation decision. Then gVisor's Sentry *is* the plan, and the honest move is to run gVisor itself, not reimplement it. |
| `mm`, `memmap`, `pgalloc`, `loader` | Address spaces and ELF loading. godeep has one address space and the Go GC. |
| `platform/systrap`, `ptrace`, `kvm` | These trap app syscalls on a Linux host. (`platform/kvm` + `pkg/ring0`, Go code running in ring 0, is interesting reading, but TamaGo already solved running Go on bare metal.) |
| `kernel` signals, futex, ptrace, cgroups, namespaces | Replaced by context cancellation, Go's `sync`, `ps -g`, and capabilities (see §6). |

---

## 5. Storage: three options, in order

| Option | How | Persistent? | Effort | Notes |
|---|---|---|---|---|
| **S1. memfs** | godeep `kernel/vfs/memfs` (shaped like gVisor tmpfs) | no | small | Needed anyway for `/tmp`, and as the test double for everything else. |
| **S2. 9P host share** ⭐ | QEMU `virtio-9p-device` exports a Mac folder. godeep writes a small virtio-9p transport (on `tamago/kvm/virtio`) plus a 9P2000.L client. | yes (files live on your Mac) | medium | **Best developer experience**: edit on the Mac, read and write in godeep, no disk images. `github.com/Harvey-OS/ninep` is already in tamago-example's dependency tree and is a starting point for the 9P message encoding. |
| **S3. virtio-blk + a filesystem** | Write a virtio-blk driver (~300 lines on `tamago/kvm/virtio`: one request queue, sector read/write), expose it as `io.ReaderAt`/`io.WriterAt`, then mount a filesystem on it. | yes (a disk image) | medium–large | **FAT32** is the pragmatic first filesystem: simple, and your Mac can create and read the images (`hdiutil`, `brew install dosfstools mtools`). Check pure-Go libraries such as `github.com/diskfs/go-diskfs` for whether they build on `GOOS=tamago` (some reach for `os`/`syscall` device calls). ext4 read-only or a custom log-structured filesystem come later. |

QEMU flags (all available in your Homebrew QEMU):

```sh
# S2: share ./share from the Mac as 9P tag "host"
-fsdev local,id=fs0,path=$(PWD)/share,security_model=none \
-device virtio-9p-device,fsdev=fs0,mount_tag=host

# S3: raw disk image on virtio-blk
-drive file=disk.img,if=none,format=raw,id=d0 -device virtio-blk-device,drive=d0
```

**Not available on macOS QEMU:**
- **virtiofs**: it needs `virtiofsd` and vhost-user on a Linux host.
- **vsock**: no `vhost-vsock` on macOS.

Both are better than 9P/TCP on Linux, so keep the driver interfaces general enough to add them later.

---

## 6. System-call families: what's worth pursuing

The **godeep equivalent** column describes what the capability method or mechanism would be. "Already" means it works today.

| Family | Linux syscalls | godeep equivalent | Reuse from | Value | Effort | Verdict |
|---|---|---|---|---|---|---|
| **Ethernet / TCP / UDP** | `socket bind listen accept connect send* recv* shutdown setsockopt` | `cap.Net` → Go `net` → `net.SocketFunc` → gVisor netstack → virtio-net | gVisor `tcpip`, `usbarmory/go-net` | ★★★★★ | S | ✅ **Phase 4, first** |
| DNS | (libc resolver) | Go's pure-Go resolver via `net.SetDefaultNS` | std lib | ★★★★ | XS | ✅ comes with networking |
| **Files & directories** | `open read write close lseek pread pwrite stat fstat getdents mkdir unlink rename truncate fsync` | `cap.FS` (`io/fs` + write extension) over godeep VFS | gVisor `vfs` (design) | ★★★★★ | M | ✅ **Phase 3** |
| **Mounts** | `mount umount pivot_root chroot` | `vfs.Mount(path, fs.FS)`; chroot = handing out a sub-rooted `cap.FS` | gVisor `vfs` mount table (design) | ★★★★ | S | ✅ with VFS |
| **procfs** | `/proc/*` reads | `/proc/<pid>/status`, `/proc/<pid>/goroutines` generated from `proc.Table` / `Snapshot()` | gVisor `kernfs`, `fsimpl/proc` (design) | ★★★★ | S | ✅ makes `ps` just `cat /proc/*/status` |
| **devfs** | `/dev/null zero random tty`, `ioctl` | each device is an `fs.File`; tty settings are methods, not ioctls | gVisor `fsimpl/dev` (design) | ★★★ | S | ✅ |
| Host file sharing | (9P / virtiofs mounts) | virtio-9p + 9P client → `fs.FS` | `Harvey-OS/ninep` | ★★★★ | M | ✅ **best dev loop** |
| Block storage | `/dev/vda`, `pread/pwrite` on a disk | virtio-blk driver → `io.ReaderAt/WriterAt` → FAT32 | `tamago/kvm/virtio`; a pure-Go FAT library | ★★★★ | M–L | ✅ after 9P |
| **Pipes** | `pipe pipe2 dup2` | channels / `io.Pipe` between processes' stdio; the shell's `a \| b` | std lib | ★★★★ | S | ✅ Phase 3 |
| Process lifecycle | `fork exec clone exit wait4 kill getpid getppid` | `proc.Spawn`, `p.Go`, `Kill`, `Wait`, `p.PID` | — | — | — | ✔️ **already** |
| Time | `clock_gettime nanosleep timerfd` | Go `time` (TamaGo TSC/LAPIC timer) | std lib | ★★★ | XS | ✔️ already (wall clock needs RTC/NTP: TamaGo has `soc/intel/rtc`; NTP once networking works) |
| Randomness | `getrandom`, `/dev/urandom` | `crypto/rand` (TamaGo `amd64/rng.go` uses RDRAND); add `virtio-rng-device` as a second entropy source | std lib, TamaGo | ★★★ | XS | ✔️ already |
| Unix-domain sockets | `socketpair`, `AF_UNIX` | channels (typed!) / `ipc.Port` named services | — | ★★★ | S | ✅ as `ipc.Port`, not sockets |
| Polling | `select poll epoll_*` | goroutines + Go `select` | — | — | — | ❌ unnecessary |
| Memory | `mmap munmap brk mprotect madvise` | `make([]byte, n)`; the GC frees it; per-process quotas later | — | ★ | — | ❌ not needed; quotas in Phase 6 |
| Shared memory | `shmget mmap(MAP_SHARED) memfd` | pass a slice or a channel; it's one address space | — | ★ | — | ❌ |
| Threads / sync | `futex clone(CLONE_THREAD) sched_yield` | goroutines, `sync`, channels | — | — | — | ❌ the runtime has it |
| Signals | `kill sigaction sigprocmask` | `ctx` cancellation + an optional `Signals` channel per process | — | ★★ | S | ◐ only SIGTERM/SIGINT-like events, as a channel |
| Users / permissions | `setuid chmod chown capset` | capabilities; file modes only as metadata | — | ★ | — | ❌ capabilities replace it |
| Namespaces / cgroups | `unshare setns` + cgroupfs | capabilities (what you can see) + per-process quotas (what you can use) | — | ★★ | M | ◐ quotas in Phase 6 |
| Debugging | `ptrace`, `/proc/pid/mem` | `ps -g`, `/proc/<pid>/goroutines`, pprof over HTTP once networking works | std `runtime/pprof`, `net/http/pprof` | ★★★★ | XS | ✅ cheap and very godeep-specific |
| Linux ABI (run ELF binaries) | all of the above, bit-for-bit | — | gVisor Sentry | — | XXL | ❌ out of scope (Go-only decision) |

Effort: XS = hours, S = days, M = 1–3 weeks, L = a month+, XXL = a separate project.

---

## 7. Suggested order

| Step | Deliverable | Demo |
|---|---|---|
| **N1** | virtio-mmio **slot probing** in `board/microvm` (prerequisite for multiple devices) | `lsdev` program lists net / blk / 9p / rng devices |
| **N2** | **Networking**: go-net virtio-net + gVisor stack + `net.SocketFunc`, QEMU user-net with port forwards, `cap.Net` | `curl localhost:8080` from your Mac returns a page served by a godeep process; `httpd` shows up in `ps` |
| **N3** | **SSH shell** using `golang.org/x/crypto/ssh` (tamago-example has a working server to adapt) | `ssh -p 2222 localhost` gets you a `godeep$` prompt; two shells at once, each its own process |
| **N4** | **VFS + memfs + procfs + devfs**, `cap.FS`, shell `ls cat echo>`, pipes | `cat /proc/3/goroutines`; `ps \| cat` |
| **N5** | **virtio-9p** host share mounted at `/host` | edit `share/hello.txt` on the Mac; `cat /host/hello.txt` in godeep |
| **N6** | **virtio-blk + FAT32** mounted at `/disk` | files survive a reboot |
| N7 | `net/http/pprof` + NTP time | profile the kernel from your Mac's browser; correct wall clock |

N1–N3 together are roughly the network stage (Phase 4) of `docs/architecture.md`, and N4 is roughly Phase 3. Doing networking first makes everything after it easier to debug (SSH, pprof).

---

## 8. References

- gVisor: `pkg/tcpip` (netstack), `pkg/sentry/vfs`, `pkg/sentry/fsimpl/{kernfs,proc,tmpfs,erofs,gofer,overlay}`, `pkg/sentry/kernel` (Task-as-goroutine). Apache-2.0. Use the `go` branch for `go get`-able sources.
- `github.com/usbarmory/go-net`: TamaGo NIC drivers (virtio-net, i.MX ENET, USB) and the gVisor/lneto stack adapters.
- `github.com/usbarmory/tamago-example`, `network/`: a working microvm + gVisor + SSH + HTTP setup to copy from.
- tamago-go `src/net/net_tamago.go` (`net.SocketFunc`), `src/syscall/fs_tamago.go` (built-in in-memory FS, `MkDev`).
- `tamago/kvm/virtio`: virtio 1.2 MMIO/PCI transports and virtqueues for writing blk / 9p / rng drivers.
- virtio spec v1.2: §5.1 network, §5.2 block, §5.4 entropy, §5.3 console; 9P2000.L protocol docs.
- `GoGpuSupport.md`: the GPU host-offload option depends on N2.
