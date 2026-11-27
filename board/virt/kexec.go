//go:build tamago && arm64 && !applem1

package virt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"

	"godeep/kernel/dev"
)

// kexec: boot another godeep Image without going through QEMU (the shell's
// kexec VOLUME:PATH). The kernel has already stopped every process and
// driver; this quiesces what could still write memory or interrupt (virtio
// devices, PCI bus masters, the GIC, the timer), stages the Image in the
// DMA region, cleans the data cache to the point of coherency over all of
// RAM, turns the MMU off and jumps to the Image with the device tree in x0
// (the Linux arm64 boot protocol, like QEMU's -kernel). The new kernel's
// entry relocates itself to its link address.
//
// One core only: secondaries run Go Ms and would have to stop themselves
// (PSCI CPU_OFF), which isn't done yet.

const kexecStage = DMA_BASE + 0x400000 // 2 MB aligned, above the device tree copy

// defined in kexec_arm64.s
func kexec_jump(entry, fdt, ram0, ram1, dma0, dma1 uint64)

// virtioBases are the virtio-mmio slots, reset before the jump.
var virtioBases []uint64

func noteVirtio(nodes []dev.Node) {
	for _, n := range nodes {
		if len(n.Reg) > 0 && (n.Compatible == "virtio,mmio" || len(n.Compatible) > 7 && n.Compatible[:7] == "virtio,") {
			virtioBases = append(virtioBases, n.Reg[0].Base)
		}
	}
}

// kexecCheck says whether img can be booted now.
func kexecCheck(img []byte) error {
	if n := atomic.LoadUint32(&smpN); n > 1 {
		return fmt.Errorf("kexec needs one core (%d running): boot with SMP=1", n)
	}
	if len(img) < 64 || string(img[0x38:0x3c]) != "ARM\x64" {
		return errors.New("not an arm64 Image (make image)")
	}
	size := binary.LittleEndian.Uint64(img[16:])
	if size < uint64(len(img)) || kexecStage+size > PSTORE_BASE {
		return fmt.Errorf("image_size %d doesn't fit the staging area", size)
	}
	return nil
}

// kexec boots img; it returns only if it can't.
func kexec(img []byte) error {
	if err := kexecCheck(img); err != nil {
		return err
	}
	daif_mask_irq()
	write_cntv_ctl(0)
	for _, b := range virtioBases {
		w32(b+0x70, 0) // Status = 0: device reset, stops its DMA
	}
	quiescePCI()
	w32(GICD_BASE, 0) // GICD_CTLR: distributor off
	for bank := uint64(0); bank < 32; bank++ {
		w32(GICD_BASE+gicdICENABLER+4*bank, 0xffffffff)
	}
	w32(GICR_BASE+gicrSGIBase+gicdICENABLER, 0xffffffff) // this core's SGIs/PPIs

	size := binary.LittleEndian.Uint64(img[16:])
	stage := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(kexecStage))), size)
	n := copy(stage, img)
	clear(stage[n:]) // bss
	kexec_jump(kexecStage, FDT_RESERVE_BASE, DRAM_BASE, DRAM_BASE+ramSize, DMA_BASE, DMA_BASE+DMA_SIZE)
	return errors.New("kexec: returned")
}

// FDT_RESERVE_BASE is where cpuinit keeps the boot device tree.
const FDT_RESERVE_BASE = DMA_BASE

// quiescePCI clears Bus Master Enable on every PCI function (ECAM, buses
// 0-15): no NVMe or xHCI DMA into the next kernel's memory.
func quiescePCI() {
	for bus := uint64(0); bus < 16; bus++ {
		for devfn := uint64(0); devfn < 256; devfn++ {
			cfg := PCIE_ECAM + bus<<20 | devfn<<12
			if r32(cfg)&0xffff == 0xffff {
				continue
			}
			cmd := r32(cfg + 4)
			w32(cfg+4, cmd&0xffff&^(1<<2)) // command without BME; status bits written 0
		}
	}
}
