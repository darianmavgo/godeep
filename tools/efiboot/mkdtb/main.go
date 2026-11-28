// Command mkdtb writes the device tree tools/efiboot passes to godeep on
// Parallels Desktop (docs/Parallels.md). Parallels describes its arm64 VMs
// only with ACPI; this tree carries what board/virt needs, transcribed
// from the ACPI tables of Parallels Desktop 27.1 (efiboot's probe):
//
//	MADT  GICv3: distributor 0x02010000, redistributors 0x02500000
//	      (0x20000 per core), MPIDRs 0..N-1
//	FADT  PSCI over SMC (arm_boot_flags = PSCI compliant, not HVC)
//	GTDT  virtual timer PPI 27
//	SPCR  PL011 at 0x02110000, GSI 32 (DSDT COM1)
//	MCFG  ECAM 0x02300000, bus 0; DSDT PCI0 window 0x10000000-0x1fffffff
//	UEFI  RAM 0x40000000-0x13fffffff (4 GB VM); GOP framebuffer
//
// The framebuffer's address, size and geometry come from GOP at boot:
// efiboot replaces the placeholder cells (0xfbfb000N) with GOP's values.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"godeep/kernel/fdt"
)

// Placeholders efiboot patches (big-endian cells in the struct block).
const (
	phWidth  = 0xfbfb0001
	phHeight = 0xfbfb0002
	phStride = 0xfbfb0003
	phBaseHi = 0xfbfb0004
	phBaseLo = 0xfbfb0005
	phSizeHi = 0xfbfb0006
	phSizeLo = 0xfbfb0007
)

func main() {
	out := flag.String("o", "bin/parallels/parallels.dtb", "output file")
	cpus := flag.Int("cpus", 4, "cores (the VM's cpu count)")
	memMB := flag.Uint64("mem", 4096, "RAM in MB (the VM's memory size)")
	bootargs := flag.String("bootargs", "", "/chosen/bootargs")
	pci := flag.Bool("pci", true, "describe the PCI host bridge (xHCI, virtio-net)")
	flag.Parse()

	const gicPhandle = 1
	var b fdt.Builder
	b.Begin("")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.PropStrings("model", "Parallels ARM Virtual Machine")
	b.PropStrings("compatible", "parallels,arm-vm")
	b.PropU32("interrupt-parent", gicPhandle)

	b.Begin("chosen")
	b.PropU32("#address-cells", 2)
	b.PropU32("#size-cells", 2)
	b.Prop("ranges", nil)
	b.PropStrings("stdout-path", "/pl011@2110000")
	if *bootargs != "" {
		b.PropStrings("bootargs", *bootargs)
	}
	b.Begin("framebuffer@0")
	b.PropStrings("compatible", "simple-framebuffer")
	b.PropU32("reg", phBaseHi, phBaseLo, phSizeHi, phSizeLo)
	b.PropU32("width", phWidth)
	b.PropU32("height", phHeight)
	b.PropU32("stride", phStride)
	b.PropStrings("format", "x8r8g8b8") // GOP PixelBlueGreenRedReserved8BitPerColor
	b.End()
	b.End()

	b.Begin("memory@40000000")
	b.PropStrings("device_type", "memory")
	b.PropRegs(2, 2, fdt.Range{Base: 0x4000_0000, Size: *memMB << 20})
	b.End()

	b.Begin("cpus")
	b.PropU32("#address-cells", 1)
	b.PropU32("#size-cells", 0)
	for i := 0; i < *cpus; i++ {
		b.Begin(fmt.Sprintf("cpu@%d", i))
		b.PropStrings("device_type", "cpu")
		b.PropStrings("compatible", "arm,armv8")
		b.PropU32("reg", uint32(i))
		b.PropStrings("enable-method", "psci")
		b.End()
	}
	b.End()

	b.Begin("psci")
	b.PropStrings("compatible", "arm,psci-1.0", "arm,psci-0.2", "arm,psci")
	b.PropStrings("method", "smc")
	b.End()

	b.Begin("intc@2010000")
	b.PropStrings("compatible", "arm,gic-v3")
	b.PropRegs(2, 2, fdt.Range{Base: 0x0201_0000, Size: 0x10000}, fdt.Range{Base: 0x0250_0000, Size: uint64(*cpus) * 0x20000})
	b.Prop("interrupt-controller", nil)
	b.PropU32("#interrupt-cells", 3)
	b.PropU32("phandle", gicPhandle)
	b.End()

	b.Begin("pl011@2110000")
	b.PropStrings("compatible", "arm,pl011", "arm,primecell")
	b.PropRegs(2, 2, fdt.Range{Base: 0x0211_0000, Size: 0x1000})
	b.PropU32("interrupts", 0, 0, 4) // SPI 0 = INTID 32, level high
	b.End()

	if *pci {
		b.Begin("pcie@2300000")
		b.PropStrings("compatible", "pci-host-ecam-generic")
		b.PropStrings("device_type", "pci")
		b.PropRegs(2, 2, fdt.Range{Base: 0x0230_0000, Size: 0x10_0000})
		b.PropU32("bus-range", 0, 0)
		b.PropU32("#address-cells", 3)
		b.PropU32("#size-cells", 2)
		// 32-bit memory window: PCI 0x10000000 = CPU 0x10000000, 256 MB
		b.PropU32("ranges", 0x0200_0000, 0, 0x1000_0000, 0, 0x1000_0000, 0, 0x1000_0000)
		b.End()
	}
	b.End()

	blob, err := b.Bytes()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, blob, 0o644); err != nil {
		log.Fatal(err)
	}
}
