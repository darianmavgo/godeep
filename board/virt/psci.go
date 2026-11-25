//go:build tamago && arm64

package virt

// PSCI function IDs (ARM DEN 0022). QEMU virt implements PSCI for the guest
// over HVC when the kernel runs at EL1 (always the case under HVF).
const psciSystemOff = 0x84000008

// defined in psci_arm64.s
func psciCall(fn uint64) uint64
