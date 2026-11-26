// Command fakem1n1 runs the Mac kernel image (bin/godeep-applem1.img) in QEMU,
// handing it over the way m1n1 does on the MacBook Pro.
//
// QEMU's sbsa-ref machine has RAM at 1 TB, the same base as the M1 Pro's
// DRAM, so the image runs unmodified at its link address. fakem1n1 supplies
// the rest of m1n1's hand-off:
//
//   - a shim (shim.asm) that QEMU starts at EL3: it drops to EL2 with
//     HCR_EL2.E2H=1, as on Apple cores, and jumps to the kernel with the
//     device tree in x0;
//   - the kernel loaded at a 2 MB-aligned address other than its link address
//     (m1n1 picks one in its heap), so self-relocation runs;
//   - an m1n1-style device tree: /memory, a framebuffer under /chosen
//     (x2r10g10b10 like the XDR panel) and in /reserved-memory, Apple-style
//     device nodes for lsdev, plus a PL011 so the console is mirrored to the
//     terminal (the Mac has none).
//
// Usage:
//
//	fakem1n1 [flags]            write shim.bin and boot.dtb to -o
//	fakem1n1 run [flags]        boot interactively (serial on this terminal)
//	fakem1n1 shot [flags]       save the framebuffer of a running VM as PNG (QMP)
//	fakem1n1 check [flags]      boot headless, wait for the shell, save a PNG, report
//
// `make qemu-applem1`, `make shot-applem1` and `make check-applem1` wrap these.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/png"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"godeep/drivers/simplefb"
	"godeep/kernel/fdt"
)

//go:generate go run gen_shim.go

// The Mac's memory map, as board/applem1 expects it (mem.go).
const (
	dramBase = 0x100_0000_0000
	dramSize = 0x4_0000_0000 // 16 GB: QEMU -m 16G

	// m1n1 lives near the bottom of DRAM. The first MB is sbsa-ref's own
	// device tree (for firmware; unused here), so start above it.
	shimAddr = dramBase + 0x20_0000
	fdtAddr  = dramBase + 0x30_0000
	lowRsvd  = 4 << 20                  // reserved for "m1n1": QEMU's tree, shim, our tree
	fbBase   = dramBase + 0x3_e000_0000 // a carve-out near the top, like iBoot's

	uartBase = 0x6000_0000 // sbsa-ref's non-secure PL011

	hcrE2H = 1 << 34
	hcrRW  = 1 << 31
)

type config struct {
	out        string
	kernelAddr uint64
	width      int
	height     int
	format     string
	e2h        bool
	fault      int
}

func (c *config) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.out, "o", "bin/fakem1n1", "output directory")
	fs.Uint64Var(&c.kernelAddr, "kernel-addr", dramBase+0x8000_0000,
		"where the Image is loaded (2 MB aligned; the kernel relocates itself from there)")
	fs.IntVar(&c.width, "width", 3456, "framebuffer width (16-inch MacBook Pro panel)")
	fs.IntVar(&c.height, "height", 2234, "framebuffer height")
	fs.StringVar(&c.format, "format", string(simplefb.X2R10G10B10), "framebuffer format")
	fs.BoolVar(&c.e2h, "e2h", true, "enter the kernel with HCR_EL2.E2H=1, as Apple cores do")
	fs.IntVar(&c.fault, "fault", 0, "make godeep fault at boot stage N (1-4) to test the fault screen (bootargs godeep.testfault=N)")
}

func (c *config) bpp() int {
	if c.format == string(simplefb.R5G6B5) {
		return 2
	}
	return 4
}

func (c *config) stride() int { return c.width * c.bpp() }

func (c *config) fbSize() uint64 { return uint64(c.stride() * c.height) }

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "":
		err = cmdPrepare(args)
	case "run":
		err = cmdRun(args)
	case "shot":
		err = cmdShot(args)
	case "check":
		err = cmdCheck(args)
	default:
		err = fmt.Errorf("unknown command %q (prepare by default, run, shot, check)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakem1n1:", err)
		os.Exit(1)
	}
}

func cmdPrepare(args []string) error {
	var c config
	fs := flag.NewFlagSet("fakem1n1", flag.ExitOnError)
	c.flags(fs)
	fs.Parse(args)
	files, err := prepare(&c)
	if err != nil {
		return err
	}
	fmt.Printf("fakem1n1: kernel at %#x, device tree at %#x, %dx%d %s framebuffer at %#x, E2H=%v\n",
		c.kernelAddr, fdtAddr, c.width, c.height, c.format, uint64(fbBase), c.e2h)
	for _, f := range files {
		fmt.Println("  " + f)
	}
	return nil
}

// cmdRun boots interactively: QEMU replaces this process, with the serial
// console on the terminal (Ctrl-A X quits) and QMP for `shot` and `make stop`.
func cmdRun(args []string) error {
	var c config
	fs := flag.NewFlagSet("fakem1n1 run", flag.ExitOnError)
	c.flags(fs)
	image := fs.String("image", "bin/godeep-applem1.img", "kernel Image")
	qemu := fs.String("qemu", "qemu-system-aarch64", "QEMU binary")
	sock := fs.String("qmp", "bin/qmp-applem1.sock", "QMP socket (for shot)")
	fs.Parse(args)
	if _, err := prepare(&c); err != nil {
		return err
	}
	path, err := exec.LookPath(*qemu)
	if err != nil {
		return err
	}
	os.Remove(*sock)
	argv := append([]string{*qemu}, qemuBase()...)
	argv = append(argv, "-display", "none", "-chardev", "stdio,id=con,mux=on,signal=off",
		"-serial", "chardev:con", "-monitor", "none", "-qmp", "unix:"+*sock+",server=on,wait=off")
	argv = append(argv, loaderArgs(&c, *image)...)
	fmt.Printf("booting %s on sbsa-ref via fakem1n1 (TCG; quit: Ctrl-A then X; screenshot: make shot-applem1)\n", *image)
	return syscall.Exec(path, argv, os.Environ())
}

// prepare writes shim.bin and boot.dtb.
func prepare(c *config) ([]string, error) {
	if c.kernelAddr%(2<<20) != 0 {
		return nil, fmt.Errorf("-kernel-addr %#x is not 2 MB aligned", c.kernelAddr)
	}
	if c.kernelAddr < dramBase+lowRsvd || c.kernelAddr+(64<<20) > fbBase {
		return nil, fmt.Errorf("-kernel-addr %#x overlaps the shim, device tree or framebuffer", c.kernelAddr)
	}
	if c.fbSize() > dramBase+dramSize-fbBase {
		return nil, fmt.Errorf("a %dx%d framebuffer doesn't fit in the carve-out", c.width, c.height)
	}
	hcr := uint64(hcrRW)
	if c.e2h {
		hcr |= hcrE2H
	}
	shim, err := patchShim(uartBase, hcr, fdtAddr, c.kernelAddr)
	if err != nil {
		return nil, err
	}
	dtb, err := deviceTree(c)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.out, 0o755); err != nil {
		return nil, err
	}
	shimPath, dtbPath := filepath.Join(c.out, "shim.bin"), filepath.Join(c.out, "boot.dtb")
	for path, data := range map[string][]byte{shimPath: shim, dtbPath: dtb} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
	}
	return []string{shimPath, dtbPath}, nil
}

// patchShim fills the shim's four trailing parameter words (shim.asm).
func patchShim(uart, hcr, dtb, kernel uint64) ([]byte, error) {
	b := append([]byte{}, shimCode...)
	if len(b) < 32 {
		return nil, errors.New("shim too short: regenerate with go generate")
	}
	p := b[len(b)-32:]
	if binary.LittleEndian.Uint64(p) != 0x6000_0000 {
		return nil, errors.New("shim parameter block not where expected: regenerate with go generate")
	}
	for i, v := range []uint64{uart, hcr, dtb, kernel} {
		binary.LittleEndian.PutUint64(p[i*8:], v)
	}
	return b, nil
}

// deviceTree builds an m1n1-style tree for the 16-inch M1 Pro.
func deviceTree(c *config) ([]byte, error) {
	var b fdt.Builder
	b.Begin("")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.PropStrings("model", "Apple MacBook Pro (16-inch, M1 Pro, 2021) [fakem1n1 on QEMU]")
	b.PropStrings("compatible", "apple,j316s", "apple,t6000", "apple,arm-platform")

	b.Begin("chosen")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Prop("ranges", nil)
	b.PropStrings("stdout-path", fmt.Sprintf("/serial@%x", uartBase))
	bootargs := ""
	if c.fault != 0 {
		bootargs = fmt.Sprintf("godeep.testfault=%d", c.fault)
	}
	b.PropStrings("bootargs", bootargs)
	b.Begin(fmt.Sprintf("framebuffer@%x", uint64(fbBase)))
	b.PropStrings("compatible", "apple,simple-framebuffer", "simple-framebuffer")
	b.PropRegs(2, 2, fdt.Range{Base: fbBase, Size: c.fbSize()})
	b.PropU32("width", uint32(c.width))
	b.PropU32("height", uint32(c.height))
	b.PropU32("stride", uint32(c.stride()))
	b.PropStrings("format", c.format)
	b.PropStrings("status", "okay")
	b.End()
	b.End()

	b.Begin(fmt.Sprintf("memory@%x", uint64(dramBase)))
	b.PropStrings("device_type", "memory")
	b.PropRegs(2, 2, fdt.Range{Base: dramBase, Size: fbBase - dramBase}) // usable: below the carve-out
	b.End()

	b.Begin("reserved-memory")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Prop("ranges", nil)
	b.Begin(fmt.Sprintf("framebuffer@%x", uint64(fbBase)))
	b.PropRegs(2, 2, fdt.Range{Base: fbBase, Size: dramBase + dramSize - fbBase})
	b.Prop("no-map", nil)
	b.End()
	b.Begin(fmt.Sprintf("m1n1@%x", uint64(dramBase)))
	b.PropRegs(2, 2, fdt.Range{Base: dramBase, Size: lowRsvd})
	b.Prop("no-map", nil)
	b.End()
	b.End()

	b.Begin("cpus")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 0)
	b.Begin("cpu@0")
	b.PropStrings("device_type", "cpu")
	b.PropStrings("compatible", "apple,icestorm")
	b.PropU32("reg", 0, 0)
	b.End()
	b.End()

	// Apple-style devices so lsdev has a Mac-like list. Nothing in godeep
	// touches them yet, and QEMU has nothing at these addresses.
	b.Begin("soc")
	b.PropStrings("compatible", "simple-bus")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Prop("ranges", nil)
	for _, d := range []struct {
		name, compat string
		base, size   uint64
	}{
		{"interrupt-controller", "apple,t6000-aic", 0x2_8e10_0000, 0xc000},
		{"serial", "apple,s5l-uart", 0x3_9b20_0000, 0x4000},
		{"spi", "apple,t6000-spi", 0x3_9b10_c000, 0x4000},
		{"i2c", "apple,t6000-i2c", 0x3_9b04_0000, 0x4000},
		{"smc", "apple,t6000-smc", 0x2_9040_0000, 0x4000},
		{"nvme", "apple,t6000-nvme-ans2", 0x3_9340_0000, 0x40000},
	} {
		b.Begin(fmt.Sprintf("%s@%x", d.name, d.base))
		b.PropStrings("compatible", d.compat)
		b.PropRegs(2, 2, fdt.Range{Base: d.base, Size: d.size})
		b.End()
	}
	b.End()

	// QEMU only: the console mirror.
	b.Begin(fmt.Sprintf("serial@%x", uartBase))
	b.PropStrings("compatible", "arm,pl011", "arm,primecell")
	b.PropRegs(2, 2, fdt.Range{Base: uartBase, Size: 0x1000})
	b.End()

	b.End()
	return b.Bytes()
}

// ---------- shot: framebuffer → PNG over QMP ----------

func cmdShot(args []string) error {
	var c config
	fs := flag.NewFlagSet("fakem1n1 shot", flag.ExitOnError)
	c.flags(fs)
	sock := fs.String("qmp", "bin/qmp-applem1.sock", "QEMU QMP socket")
	out := fs.String("png", "", "PNG to write (default <o>/screen.png)")
	fs.Parse(args)
	if *out == "" {
		*out = filepath.Join(c.out, "screen.png")
	}
	return shot(&c, *sock, *out)
}

func shot(c *config, sock, out string) error {
	q, err := dialQMP(sock)
	if err != nil {
		return err
	}
	defer q.Close()
	dump, err := filepath.Abs(filepath.Join(c.out, "fb.raw"))
	if err != nil {
		return err
	}
	if err := q.do("pmemsave", map[string]any{"val": uint64(fbBase), "size": c.fbSize(), "filename": dump}); err != nil {
		return err
	}
	return dumpToPNG(c, dump, out)
}

func dumpToPNG(c *config, dump, out string) error {
	pix, err := os.ReadFile(dump)
	if err != nil {
		return err
	}
	fb := &simplefb.Framebuffer{Pix: pix, Width: c.width, Height: c.height, Stride: c.stride(), Format: simplefb.Format(c.format)}
	if err := fb.Validate(); err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := png.Encode(f, fb.Image()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

type qmp struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialQMP(sock string) (*qmp, error) {
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("QMP %s: %w (is the VM running?)", sock, err)
	}
	q := &qmp{conn: conn, r: bufio.NewReader(conn)}
	if _, err := q.r.ReadBytes('\n'); err != nil { // greeting
		conn.Close()
		return nil, err
	}
	if err := q.do("qmp_capabilities", nil); err != nil {
		conn.Close()
		return nil, err
	}
	return q, nil
}

func (q *qmp) Close() error { return q.conn.Close() }

// do runs a command and waits for its reply, skipping asynchronous events.
func (q *qmp) do(cmd string, args map[string]any) error {
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	b, _ := json.Marshal(req)
	q.conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := q.conn.Write(append(b, '\n')); err != nil {
		return err
	}
	for {
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			return err
		}
		var resp struct {
			Return json.RawMessage        `json:"return"`
			Error  *struct{ Desc string } `json:"error"`
			Event  string                 `json:"event"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			return err
		}
		if resp.Event != "" {
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("QMP %s: %s", cmd, resp.Error.Desc)
		}
		return nil
	}
}

// ---------- check: boot headless and verify ----------

func cmdCheck(args []string) error {
	var c config
	fs := flag.NewFlagSet("fakem1n1 check", flag.ExitOnError)
	c.flags(fs)
	image := fs.String("image", "bin/godeep-applem1.img", "kernel Image")
	qemu := fs.String("qemu", "qemu-system-aarch64", "QEMU binary")
	timeout := fs.Duration("timeout", 90*time.Second, "how long to wait for the shell")
	want := fs.String("want", "board:,memory layout OK,uptime", "comma-separated strings the serial output must contain")
	fs.Parse(args)
	if _, err := prepare(&c); err != nil {
		return err
	}
	sock, err := filepath.Abs(filepath.Join(c.out, "qmp.sock"))
	if err != nil {
		return err
	}
	os.Remove(sock)
	log := filepath.Join(c.out, "serial.log")
	argv := append(qemuBase(), "-display", "none", "-monitor", "none", "-serial", "stdio",
		"-qmp", "unix:"+sock+",server=on,wait=off")
	argv = append(argv, loaderArgs(&c, *image)...)
	fmt.Printf("fakem1n1: %s %s\n", *qemu, strings.Join(argv, " "))
	cmd := exec.Command(*qemu, argv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Process.Kill()

	lf, err := os.Create(log)
	if err != nil {
		return err
	}
	defer lf.Close()
	seen := make(chan string, 1)
	exited := make(chan struct{}) // serial EOF: QEMU is gone
	go func() {                   // collect serial output; publish the full text after each read
		defer close(exited)
		var all strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				lf.Write(buf[:n])
				all.Write(buf[:n])
				select {
				case <-seen:
				default:
				}
				seen <- all.String()
			}
			if err != nil {
				return
			}
		}
	}()

	if c.fault != 0 {
		return waitFault(&c, sock, log, *timeout, exited)
	}

	wants := strings.Split(*want, ",")
	deadline := time.After(*timeout)
	text := ""
	for !containsAll(text, wants) {
		select {
		case text = <-seen:
		case <-exited:
			text = latest(seen, text)
			if containsAll(text, wants) {
				return fmt.Errorf("QEMU exited right after the shell came up; serial: %s", log)
			}
			return fmt.Errorf("QEMU exited; missing %q\nserial: %s", missing(text, wants), log)
		case <-deadline:
			text = latest(seen, text)
			png := filepath.Join(c.out, "screen.png")
			shotErr := shot(&c, sock, png)
			return fmt.Errorf("timed out after %v; missing %q\nserial: %s\nscreen: %s (%v)",
				*timeout, missing(text, wants), log, png, shotErr)
		}
	}
	time.Sleep(2 * time.Second) // let autorun finish drawing
	png := filepath.Join(c.out, "screen.png")
	if err := shot(&c, sock, png); err != nil {
		return fmt.Errorf("booted, but the screenshot failed: %w", err)
	}
	fmt.Printf("fakem1n1: OK: serial has %q\n  serial: %s\n  screen: %s\n", wants, log, png)
	return nil
}

// waitFault polls the framebuffer until the fault screen (red, the
// registers in white) appears, then checks the stage marks were drawn first.
func waitFault(c *config, sock, log string, timeout time.Duration, exited chan struct{}) error {
	png := filepath.Join(c.out, "screen.png")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return fmt.Errorf("QEMU exited before the fault screen appeared; serial: %s", log)
		case <-time.After(3 * time.Second):
		}
		if err := shot(c, sock, png); err != nil {
			continue
		}
		fb, err := readDump(c)
		if err != nil {
			return err
		}
		img := fb.Image()
		corner := img.RGBAAt(c.width-1, c.height-1)
		if corner.R > 0x80 && corner.G < 0x30 && corner.B < 0x30 {
			fmt.Printf("fakem1n1: OK: fault screen for stage %d\n  serial: %s\n  screen: %s\n", c.fault, log, png)
			return nil
		}
	}
	return fmt.Errorf("no fault screen after %v (stage %d); serial: %s, screen: %s", timeout, c.fault, log, png)
}

func readDump(c *config) (*simplefb.Framebuffer, error) {
	pix, err := os.ReadFile(filepath.Join(c.out, "fb.raw"))
	if err != nil {
		return nil, err
	}
	fb := &simplefb.Framebuffer{Pix: pix, Width: c.width, Height: c.height, Stride: c.stride(), Format: simplefb.Format(c.format)}
	return fb, fb.Validate()
}

func latest(ch chan string, cur string) string {
	select {
	case s := <-ch:
		return s
	default:
		return cur
	}
}

func containsAll(s string, subs []string) bool { return len(missing(s, subs)) == 0 }

func missing(s string, subs []string) (out []string) {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			out = append(out, x)
		}
	}
	return
}

// qemuBase is the machine: sbsa-ref (RAM at 1 TB), one core, TCG (sbsa-ref
// needs EL3, which HVF doesn't provide).
func qemuBase() []string {
	return []string{"-machine", "sbsa-ref", "-cpu", "max", "-smp", "1", "-m", "16G", "-accel", "tcg", "-no-reboot"}
}

func loaderArgs(c *config, image string) []string {
	return []string{
		"-device", fmt.Sprintf("loader,file=%s,addr=%#x", filepath.Join(c.out, "shim.bin"), uint64(shimAddr)),
		"-device", fmt.Sprintf("loader,file=%s,addr=%#x", filepath.Join(c.out, "boot.dtb"), uint64(fdtAddr)),
		"-device", fmt.Sprintf("loader,file=%s,addr=%#x", image, c.kernelAddr),
		"-device", fmt.Sprintf("loader,addr=%#x,cpu-num=0", uint64(shimAddr)),
	}
}
