package main

import (
	"encoding/binary"
	"fmt"

	"godeep/kernel/fdt"
)

// The internal keyboard's path (Asahi's t600x tree, j316s): power domains,
// two GPIO blocks, the SPI controller and the HID transport under it.
// QEMU has none of this hardware, so each block points at a RAM page in
// the "m1n1" carve-out, preloaded (regs.bin) with values that let the
// driver come up: power domains already active, the keyboard's ready line
// idle (high), and an SPI FIFO status that always shows 16 bytes to read,
// which read as zero. The driver then powers up, pulses spien, waits for
// the ready line, falls back to blind reads after 3 s and counts empty
// packets. That exercises the board wiring and the device-tree
// resolution, not the keyboard protocol (host tests cover that).
const (
	fakeRegs = dramBase + 0x3e_0000 // regs.bin: from here to the end of the carve-out
	fakePmgr = fakeRegs
	fakeAP   = fakeRegs + 0x4000
	fakeNub  = fakeRegs + 0x8000
	fakeSPI  = fakeRegs + 0xc000

	spiFIFOSTAT = 0x10c
	nubReady    = 6
)

// power domains: offset, phandle, parent phandles (as in t600x-pmgr.dtsi)
var fakeDomains = []struct {
	name    string
	off, ph uint32
	parents []uint32
}{
	{"ps_afi", 0x1d8, 100, nil},
	{"ps_afnc2_ioa", 0x148, 101, []uint32{100}},
	{"ps_afnc2_ls", 0x150, 102, []uint32{101}},
	{"ps_afnc2_lw1", 0x160, 103, []uint32{102}},
	{"ps_sio", 0x180, 104, []uint32{103}},
	{"ps_spi_p", 0x1e8, 105, []uint32{104}},
	{"ps_spi3", 0x208, 106, []uint32{105}},
	{"ps_gpio", 0x120, 107, []uint32{104}},
}

// keyboardNodes adds the keyboard path (inside /soc).
func keyboardNodes(b *fdt.Builder) {
	b.Begin(fmt.Sprintf("power-management@%x", uint64(fakePmgr)))
	b.PropStrings("compatible", "apple,t6000-pmgr", "apple,pmgr", "syscon", "simple-mfd")
	b.PropRegs(2, 2, fdt.Range{Base: fakePmgr, Size: 0x4000})
	b.PropU32("#address-cells", 1)
	b.PropU32("#size-cells", 1)
	for _, d := range fakeDomains {
		b.Begin(fmt.Sprintf("power-controller@%x", d.off))
		b.PropStrings("compatible", "apple,t6000-pmgr-pwrstate", "apple,pmgr-pwrstate")
		b.PropU32("reg", d.off, 4)
		b.PropU32("#power-domain-cells", 0)
		b.PropU32("phandle", d.ph)
		b.PropStrings("label", d.name)
		if d.parents != nil {
			b.PropU32("power-domains", d.parents...)
		}
		b.End()
	}
	b.End()

	pinctrl := func(base uint64, npins, ph uint32, power ...uint32) {
		b.PropStrings("compatible", "apple,t6000-pinctrl", "apple,pinctrl")
		b.PropRegs(2, 2, fdt.Range{Base: base, Size: 0x4000})
		b.PropU32("apple,npins", npins)
		b.PropU32("#gpio-cells", 2)
		b.PropU32("#interrupt-cells", 2)
		b.PropU32("phandle", ph)
		if len(power) > 0 {
			b.PropU32("power-domains", power...)
		}
	}
	b.Begin(fmt.Sprintf("pinctrl@%x", uint64(fakeAP))) // pinctrl_ap
	pinctrl(fakeAP, 255, 110, 107)
	b.Begin("spi3-pins")
	b.PropU32("pinmux", 52|1<<16, 53|1<<16, 54|1<<16, 55|1<<16)
	b.PropU32("phandle", 111)
	b.End()
	b.End()
	b.Begin(fmt.Sprintf("pinctrl@%x", uint64(fakeNub))) // pinctrl_nub
	pinctrl(fakeNub, 16, 112)
	b.End()

	b.Begin("clock-ref")
	b.PropStrings("compatible", "fixed-clock")
	b.PropU32("#clock-cells", 0)
	b.PropU32("clock-frequency", 24_000_000)
	b.PropU32("phandle", 113)
	b.End()

	b.Begin(fmt.Sprintf("spi@%x", uint64(fakeSPI))) // spi3
	b.PropStrings("compatible", "apple,t6000-spi", "apple,spi")
	b.PropRegs(2, 2, fdt.Range{Base: fakeSPI, Size: 0x4000})
	b.PropU32("#address-cells", 1)
	b.PropU32("#size-cells", 0)
	b.PropU32("clocks", 113)
	b.PropU32("pinctrl-0", 111)
	b.PropStrings("pinctrl-names", "default")
	b.PropU32("power-domains", 106)
	b.PropStrings("status", "okay")
	b.Begin("hid-transport@0")
	b.PropStrings("compatible", "apple,spi-hid-transport")
	b.PropU32("reg", 0)
	b.PropU32("spi-max-frequency", 8_000_000)
	b.PropU32("spi-cs-setup-delay-ns", 65000)
	b.PropU32("spi-cs-hold-delay-ns", 65000)
	b.PropU32("spi-cs-inactive-delay-ns", 250000)
	b.PropU32("spien-gpios", 110, 194, 0)
	b.PropU32("interrupts-extended", 112, nubReady, 8) // IRQ_TYPE_LEVEL_LOW
	b.End()
	b.End()
}

// regsImage is regs.bin: the preloaded register pages, up to the end of the
// carve-out (the fake watchdog page included, all zero).
func regsImage() []byte {
	img := make([]byte, dramBase+lowRsvd-fakeRegs)
	put := func(addr uint64, v uint32) { binary.LittleEndian.PutUint32(img[addr-fakeRegs:], v) }
	for _, d := range fakeDomains {
		put(fakePmgr+uint64(d.off), 0xff) // active (target and actual 0xf)
	}
	put(fakeNub+4*nubReady, 1<<9|7<<1|1) // input, interrupt off, level high (idle)
	put(fakeSPI+spiFIFOSTAT, 16<<24)     // RX FIFO: 16 bytes waiting, TX empty
	return img
}
