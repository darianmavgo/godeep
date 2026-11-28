# An Omarchy-caliber desktop on godeep

What it would take to give godeep a desktop on the level of [Omarchy](https://github.com/basecamp/omarchy), DHH's Arch + Hyprland setup, which this Mac also boots (U-Boot autoboots it). Written 2026-11-27; nothing here is built yet beyond what §1 lists.

## TL;DR

Omarchy is two things, and they have very different costs:

1. **The desktop**: a keyboard-driven tiling compositor (Hyprland), one themed shell (bar, launcher, notifications, OSDs, lock), a fast terminal, live themes, and HiDPI at 120 Hz with animations. **This can be rebuilt natively in Go on what godeep already has**: `kernel/gpu`, `kernel/wm`, `kernel/input`, the Mac's keyboard and multitouch trackpad, SMP, and `ipc`. It renders on the CPU at first, with no new hardware drivers until the polish stage. Roughly 6–10k lines of Go for a convincing G1–G2 (§11).
2. **The apps**: Chromium and the web apps, Neovim, btop, a file manager, Spotify. **These are Linux programs, and godeep can't run Linux binaries.** This is the wall. The ways around it are native Go apps, WASM apps with a window API, terminal programs inside godeep, and **remote apps**: a streaming client to another machine (Omarchy 4 itself ships Moonlight). A Linux syscall layer ("run Chromium") is a separate project bigger than everything else here put together; §10 says why.

The polish that makes Omarchy *feel* like Omarchy (blur, shadows, 120 Hz animation, vsync, brightness keys) needs this Mac's GPU (`drivers/apple/agx` stages 3–5) and display coprocessor (DCP), both big Apple-specific driver projects.

**Recommended first milestone (G1):**
- TrueType text, a theme, tiling with workspaces and Super-key binds, and a real terminal window running `sh`, all on the CPU
- developed on Parallels and QEMU, then the Mac

That alone gets the look and the keyboard flow.

## What you can see today

`wm` is a shell builtin: `wm [SECONDS]` starts the compositor (`kernel/wm`) with three demo windows: a clock, an input log and a test pattern. You can click, focus and drag them by their title bars. It's a proof of the compositor's design, not a desktop.

It needs a `gpu.Display`, and **today only QEMU's virtio-gpu publishes one**, so it runs only there:

```
make qemu QEMU_DISPLAY=cocoa     # a window on the Mac showing godeep's screen
godeep$ wm 60                    # typed in that window or the terminal; ^C ends it
```

`check-virt` drives it headlessly: it drags a window over QMP and checks the screendump. On **Parallels and the Mac**, the screen is the framebuffer console (`drivers/simplefb` on GOP or m1n1's framebuffer), which isn't a `gpu.Display`, so `wm` answers "no display". Publishing one there is the first task of G1 (§11): a `gpu.Display` on the firmware framebuffer, handing the screen back to the console when the compositor exits.

## 1. Omarchy, piece by piece

Omarchy's component list changed with 4.0 (2026). A draft 4.0 pull request and release notes describe one integrated Quickshell shell replacing Waybar, Walker, Mako, SwayOSD and the lock tools, and Foot as the default terminal. Older docs and articles still describe Waybar, Walker and Alacritty. **Check your own install** (`omarchy-version`, `~/.config/`) before treating the rows below as exact.

| Omarchy | Role | godeep counterpart | Status |
|---|---|---|---|
| Hyprland | tiling compositor: dwindle/master layouts, workspaces, binds, gaps, rounded borders, blur, shadows, animations | `kernel/wm` (447 lines): floating windows, one goroutine owns the scene, damage-based CPU compositing, click-to-focus, drag by the title bar | **extend** (tiling, workspaces, binds, decorations); effects need the GPU |
| Quickshell shell (4.0) / Waybar | bar: workspaces, clock, CPU, battery, network, volume | `dev.CPULoad` (per-core load), SMC keys (`BUIC` battery, lid) in `drivers/apple/smc`, netd's `ifconfig`, `ntpd` time | **new** (data sources exist) |
| … / Walker | launcher with fuzzy search | `prog.All()` + shell builtins, `ipc` ports | **new** |
| … / Mako, SwayOSD | notifications, volume/brightness OSD | an `ipc` port service; no audio or brightness drivers yet | **new**; OSD values need DCP and audio |
| … / hyprlock, hypridle | lock screen, idle | a lock surface on top; no secret store, and no entropy on the Mac yet | **new**; real security needs entropy |
| Foot / Alacritty | GPU terminal, true color, Nerd fonts | `drivers/simplefb` console: bitmap font, scrollback, a minimal ANSI subset (`ansi.go`, 144 lines: cursor moves, clear, reverse video); `tty.NewFor` + `sh` (as sshd sessions) | **extend**: an xterm-class VT and TrueType text |
| Themes | palette + wallpaper, switched live | `wm`'s colors are package variables | **new**: a theme struct and switching |
| Fonts (JetBrains Mono Nerd Font, …) | anti-aliased TrueType | `golang.org/x/image` already a dependency (`basicfont` in wm) | **extend**: opentype + glyph cache |
| HiDPI, 120 Hz ProMotion | crisp text, smooth motion | m1n1's framebuffer: 3456x2234, `x2r10g10b10`, fixed mode, no vsync | **wall until DCP** |
| Chromium + web apps | browser, Basecamp/HEY/… as apps | none | **wall** (§10) |
| Neovim, btop, lazygit, … | terminal apps | Go builtins and programs, WASI programs (wazero), godeep's own `edit` | partial: native equivalents only |
| Moonlight (4.0) | game/desktop streaming client | none | **new**: the realistic route to Linux and macOS apps (§10) |

## 2. The display stack

**What draws today:**
- `kernel/gpu` defines `Display` and `Surface` (XRGB8888 pixels + `Present(damage)`).
- Back ends:
  - `drivers/virtio/gpu` (QEMU 2D)
  - `drivers/simplefb` on m1n1's framebuffer (the Mac) or on UEFI GOP (Parallels)
  - ramfb (QEMU)
- Every pixel is written by the CPU.

**The CPU budget on the Mac.** A full frame at 3456x2234 is 7.7 Mpixels, 31 MB at 4 bytes per pixel. Redrawing everything at 120 Hz moves 3.7 GB/s just to write the framebuffer, before compositing reads. The M1 Pro's memory system is rated at 200 GB/s, but godeep has never measured what its CPU code gets.

(`burn` reports SHA-256 throughput, not memory bandwidth, so it doesn't answer this. The first G1 task is an `fbbench` that fills and blits the framebuffer from 1, 4 and 10 goroutines.)

The design consequences are the same whatever the number turns out to be:
- **Damage tracking everywhere.** `wm` already composes only damaged rectangles; tiling layouts, the bar and the terminal must report small damage, e.g. one terminal line.
- **Split composition across cores.** One goroutine owns the scene, but the blend of a damaged region can be cut into horizontal bands and fanned out with `p.Go`, the SMP speed-up `burn` shows. Mind TamaGo's no-preemption rule: tight loops `runtime.Gosched()`.
- **Effects stay modest on the CPU:**
  - cheap: rounded corners, 1–2 px borders, solid shadows
  - prohibitive at this resolution: blur, and full-screen fades at 120 Hz
- **60 Hz is the realistic CPU target**, and only with small damage.
- **The 10-bit framebuffer** (`x2r10g10b10`) means converting from XRGB8888 on present, or composing in 10-bit; simplefb already handles both formats.

**The GPU path (polish).**
- `drivers/apple/agx` has stages 1–2 built: bring-up and firmware boot (`docs/AGX.md`), not yet run on the Mac. Stage 3 is compute jobs, stage 4 shaders (precompiled with Mesa), stage 5 rendering plus a GPU-backed `gpu.Display`.
- With stage 5, compositing moves to the GPU: textured quads per window, blur and shadows as shaders, and the CPU freed for clients. That's what makes Hyprland-style effects affordable.
- It's the largest single item in this document: Asahi's driver is about 17,700 lines of Rust, plus Mesa's shader compiler, which godeep would use offline.

**DCP (the display coprocessor).** Mode setting, vsync, the 120 Hz ProMotion modes, brightness and the backlight all belong to DCP, another RTKit coprocessor with a large, version-specific protocol (Asahi's `dcp` driver). Until it exists:
- the screen is whatever m1n1 set up
- there's no vsync (tearing is possible) and no brightness keys
- there's no external display

It's independent of AGX and can come first: vsync and brightness matter more to feel than blur does.

## 3. The compositor: from `wm` to "hypr-like"

`kernel/wm` already has the right shape:
- one goroutine owns the scene; other goroutines hand it closures over a channel (`Manager.do`)
- windows are `gpu.Surface`s their clients draw into
- `Present` copies the damaged part
- a window dies with its ctx, i.e. when its process ends
- input arrives as `input.Event`s and goes to the focused window or the one under the pointer

**What to add:**
- **Layouts:** Hyprland's dwindle (binary split) and master/stack, per workspace; floating as a per-window flag. Layout is a pure function from window list and screen to rectangles, host-testable.
- **Workspaces:** numbered, Super+1..9 to switch, Super+Shift+N to move a window, with the bar showing them.
- **Bind table:** `Super+Return` terminal, `Super+Space` launcher, `Super+W` close, arrows for focus and move, `Super+F` fullscreen. Omarchy's binds are a good default set. `kernel/input` already reports modifiers including `ModLGUI` (Super), and the Mac keyboard reports the Command key there.
- **Decorations:** gaps, rounded anti-aliased corners, a focused-window border in the theme's accent. No title bars in tiling mode (Hyprland has none).
- **Animations:** windows slide or scale on open and on workspace switch, driven by the compositor goroutine's own frame ticker. Keep them short and damage-bounded on the CPU.
- **Gestures:** the Mac trackpad already reports multitouch (`spihid`, `Config.Trackpad`); a 3-finger horizontal swipe can switch workspaces, as in Omarchy.
- **Resizing** splits with Super+drag or binds.

**Rough size:**

| Part | Lines |
|---|---|
| Layouts, workspaces and binds | 1–1.5k, plus tests |
| Decorations and animations | 0.5–1k |

## 4. A window API for programs

Hyprland speaks Wayland because its clients are separate processes in separate address spaces. godeep's clients are Procs in one memory-safe address space, so there's no wire protocol to copy:
- **In-kernel clients** (Go programs and builtins) get a window through a capability: a `wm` port in the Proc's `ipc.Namespace`, as `kvd`/`kv` do today. `NewWindow(ctx, title, w, h)` returns a `gpu.Surface` plus an event channel. The window dies with the Proc, and `restrict` can withhold the capability.
- **WASM clients** (the ecosystem route: Go, Rust, Zig, C compiled to `wasm`) get host functions in a `godeep:gui` module:
  - `window_open`, `window_pixels` (a region of linear memory the host copies on present), `present(x, y, w, h)`, `poll_event`
  - added to `programs/wasm` beside WASI
  - a WASM program then needs no knowledge of godeep's internals
- **No Wayland or X11 server.** That only pays off together with Linux apps (§10).

About 0.5–1k lines plus a small WASM-side SDK (a Go package for `GOOS=wasip1` programs).

## 5. Text and fonts

Text quality is most of what makes a desktop look modern, and the console's bitmap font (and `wm`'s `basicfont` 7x13) is the most visible gap:
- **Rasterizing:** `golang.org/x/image/font/opentype` + `sfnt` + `vector` (pure Go, already a module dependency) rasterize TrueType/OpenType with anti-aliasing. Add a per-size glyph cache (an alpha mask per glyph) and an alpha-blend into surfaces.
- **Fonts:** embed one family in the image, e.g. JetBrains Mono Nerd Font, the Omarchy default, Regular and Bold, about 2–4 MB. It provides the Nerd icons the bar uses, which saves an icon system.
- **Shaping:** monospace terminal text doesn't need it. Proportional UI text with ligatures, emoji and complex scripts needs [go-text/typesetting](https://github.com/go-text/typesetting), a pure-Go HarfBuzz port. Check that it builds for tamago (it should: no cgo).
- **HiDPI:** render at scale 2 on the Mac (3456x2234 is a 1728x1117 "points" screen), set per display.

About 0.5k lines for the rasterizing and cache layer.

## 6. The terminal

Omarchy lives in its terminal, so this matters most after the compositor:
- **A VT parser** at xterm level: SGR colors (16, 256, 24-bit), bold and italic, alternate screen, scroll regions, bracketed paste, mouse reporting, OSC titles. Today's `drivers/simplefb/ansi.go` handles a minimal subset.
- Write it as a pure `kernel/vt` package: a parser plus cell grid, host-tested against recorded `vttest`-style streams. Use it for both the framebuffer console and terminal windows.
- **A terminal window** is a `wm` window whose cells are drawn with the glyph cache, running `sh` on `tty.NewFor` exactly as `programs/sshd` sessions do. Each window is a Proc, so `ps`, `kill` and `restrict` apply.
- **Programs inside it:** godeep's shell, builtins and WASI programs. Neovim and btop aren't godeep programs; with a full VT, though, `ssh othermachine` from a godeep terminal runs them remotely, which is how much Omarchy-style work could happen early on.

About 1.5–2k lines (the VT is most of it).

## 7. The shell: bar, launcher, notifications, OSD, lock

Each is a client of the compositor (layer-shell-like: anchored, above or below tiled windows) and a Proc of its own:
- **Bar:**
  - workspaces (from wm), clock (NTP-set time; the Mac has no RTC driver)
  - CPU per core from `dev.CPULoad`, memory from `runtime.MemStats`
  - network from netd (`ifconfig`'s data), battery and AC from SMC keys on the Mac
  - one goroutine per module, redrawing only its own rectangle
- **Launcher:** Super+Space opens a centered list, fuzzy matching over `prog.All()` (programs), shell builtins and `.wasm` files on mounted volumes. Enter starts it in a new terminal or window.
- **Notifications:** a `notify` port in the namespace; any Proc with the capability posts one. Toasts in a corner, with history.
- **OSD:** volume and brightness bars, once there's something to control (audio driver, DCP backlight).
- **Lock:** a full-screen surface that grabs input. Real locking needs a stored secret, a key-derivation function and entropy; the Mac has no entropy source yet (`CLAUDE.md`).

About 1.5–2.5k lines.

## 8. Theming

Omarchy themes are a palette plus a wallpaper plus per-app config. godeep needs:
- a `Theme` struct: background, foreground, accent, the 16 terminal colors, border, gaps, radius, font
- a few built-in themes, e.g. Tokyo Night and Catppuccin, transcribed from Omarchy's palette files
- a wallpaper decoded with `image/png` or `image/jpeg` (stdlib), pre-scaled once
- live switching: the compositor redraws everything once, and terminals re-map their palette

About 0.3k lines plus theme files.

## 9. A toolkit for apps

Settings panes, a file browser, a system monitor: anything beyond a terminal needs widgets. Off-the-shelf Go toolkits assume an OS graphics stack:
- **Fyne and Ebiten** need OpenGL.
- **Gio** has GPU renderers and a CPU fallback; whether that fallback builds without cgo for tamago is unverified.

The pragmatic route is a small **immediate-mode** widget set drawn on `gpu.Surface` with the font layer and `golang.org/x/image/vector`: labels, buttons, lists, text input, scroll views, split panes. It's written once and used by native and WASM apps alike (through the WASM SDK). About 2–3k lines for a useful set.

## 10. The walls

- **Linux apps (Chromium, Electron apps, Neovim, Spotify).**
  - What running them would take: a Linux syscall layer (ELF loader, syscall dispatch to Procs and capabilities, `mmap` and threads). That cuts against godeep's design (no MMU isolation, Go's memory safety stands in for it), and native code would have to run in the shared address space with no isolation at all.
  - Wayland or X11 would come on top, plus GPU drivers for Chromium (or its slow software path).
  - This isn't a port; it's a second operating system. **Not recommended.**
- **A web browser.**
  - Chromium, Firefox and WebKit are out (above). A pure-Go HTML/CSS renderer good enough for real sites doesn't exist; writing one is a multi-year project.
  - Realistic options:
    - **remote**: a streaming client (Moonlight/GameStream or VNC/RDP, in Go) showing a browser running on another machine, such as Omarchy on this same Mac's other boot, a home server, or the Windows PC
    - **text**: a terminal-mode HTML reader on `curl`
- **Video playback:** there are pure-Go decoders for some formats, but not modern codecs at speed on the CPU. Hardware decode (the M1's AVD) is another Apple coprocessor driver. Remote streaming sidesteps it, since the stream is decoded… also on the CPU. Plan on low resolution or remote-only for a long time.
- **Audio:** the Mac's speakers and headphone jack are a large driver stack in Asahi (the ADMAC DMA controller, TAS2764 speaker amplifiers, the headphone codec, plus speaker-safety DSP to avoid damaging the speakers). A USB audio class driver (USB DACs and headsets, over the existing xHCI) is much smaller and would give sound first.
- **Remote apps as the bridge.** A Go **Moonlight client** (the protocol is documented by the open-source Moonlight/Sunshine projects: RTSP setup, H.264/HEVC video, Opus audio, input over ENet) or a simpler **VNC client** (RFB is small) would show any Linux or macOS app in a godeep window:
  - VNC: about 1.5k lines with raw/ZRLE encodings
  - Moonlight: needs an H.264 decoder in Go, a big item on its own

  This is the one route to "Chromium on godeep" that's measured in weeks rather than years.

## 11. Roadmap

| Milestone | Contents | Size (rough) | Checked by |
|---|---|---|---|
| **G1 Look and flow** | a `gpu.Display` on the firmware framebuffer (Parallels GOP, the Mac's m1n1 framebuffer), so `wm` runs there; `fbbench`; font layer + glyph cache + embedded Nerd font; `Theme`; tiling layouts, workspaces, Super binds, gaps and borders in `wm`; `kernel/vt` + terminal windows; Super+Return / Super+W | 4–6k lines | host tests (layouts, VT streams, glyph cache); `check-wm`-style screendump pixel checks on QEMU (`tools/pngpix`); `prlctl capture` on Parallels |
| **G2 Shell** | bar (workspaces, clock, CPU, net, battery on the Mac), launcher, notifications port, lock screen (cosmetic until entropy) | 2–3k | the same; on the Mac, the bar shows battery from SMC |
| **G3 Apps** | window capability for Procs; `godeep:gui` WASM host module + Go SDK; immediate-mode widgets; a sample app or two (system monitor on `dev.CPULoad`, file browser on vfs) | 3–4k | a WASM GUI app pushed with `make push` runs in a window |
| **G4 Display** | DCP driver: vsync, brightness keys, 120 Hz modes, external display later | large (Asahi's DCP driver is substantial; the protocol is version-specific like AGX's) | on the Mac: brightness keys work, no tearing |
| **G5 GPU** | AGX stages 3–5, a GPU `gpu.Display`, then GPU composition: blur, shadows, animations at 120 Hz | very large | `docs/AGX.md`'s per-stage checks, then `wm` on the GPU |
| **G6 Remote apps** | VNC client window, then Moonlight (with H.264 decode) | 1.5k (VNC) to large (Moonlight) | a browser on another machine, in a godeep window |

G1 and G2 give the Omarchy *experience* minus the effects and the apps. G3 makes godeep a platform. G4–G5 are the polish and depend on Apple-specific reverse-engineered drivers. G6 is the practical answer to "my apps".

## 12. Where to develop it

- **Parallels and QEMU first:**
  - Both give a framebuffer: GOP on Parallels, ramfb or virtio-gpu on QEMU.
  - Input: USB keyboard and tablet in both, and the Parallels window passes the Mac's keyboard and trackpad through.
  - Screenshots for checks: `prlctl capture`, QMP `screendump`.
  - Boot-to-test in seconds (`make hot`, `make parallels`).
  - Parallels shows real multi-core composition costs (`top`'s per-core line).
- **The Mac** for what only it has: HiDPI at 3456x2234, the 10-bit framebuffer, real trackpad gestures, battery and lid from SMC, and eventually DCP and AGX. The Mac boot itself is still settling (`docs/TargetMyMacProgress.log`), so GUI work shouldn't wait on it.

## Sources

- Omarchy: [github.com/basecamp/omarchy](https://github.com/basecamp/omarchy); the 4.0 "Quattro" changes (an integrated shell replacing Waybar, Walker, Mako, SwayOSD and the lock tools; Foot as the default terminal) are from a [draft pull request](https://github.com/basecamp/omarchy/pull/6231) and the [v4.0.0 release listing](https://sourceforge.net/projects/omarchy.mirror/files/v4.0.0/). Other write-ups ([starryhope](https://www.starryhope.com/linux/what-is-omarchy/), [The New Stack](https://thenewstack.io/what-is-omarchy-linux-and-why-is-37signals-moving-to-it/)) describe earlier versions, so check the details against your install.
- Hyprland: [wiki.hyprland.org](https://wiki.hyprland.org) (layouts, binds, decorations).
- This Mac's GPU and display: `docs/AGX.md` (Asahi `drm/asahi`, m1n1) and Asahi's DCP driver.
- Go libraries: [golang.org/x/image](https://pkg.go.dev/golang.org/x/image) (opentype, vector), [go-text/typesetting](https://github.com/go-text/typesetting).
