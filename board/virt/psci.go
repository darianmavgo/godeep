//go:build tamago && arm64 && !applem1

package virt

// PSCI function IDs (ARM DEN 0022). QEMU virt implements PSCI for the guest
// over HVC when the kernel runs at EL1 (always the case under HVF).
const psciSystemOff = 0x84000008

// defined in psci_arm64.s
func psciCall(fn uint64) uint64    // HVC conduit
func psciCallSMC(fn uint64) uint64 // SMC conduit

// psciMethod is the conduit from the device tree's /psci "method" property:
// "hvc" (QEMU virt at EL1, the default) or "smc" (QEMU with EL2, where HVC
// belongs to the hypervisor level). Apple Silicon has no PSCI at all.
var psciMethod = "hvc"

func psciPowerOff() {
	if psciMethod == "smc" {
		psciCallSMC(psciSystemOff)
	} else {
		psciCall(psciSystemOff)
	}
}

func psciCall4(fn, a1, a2, a3 uint64) uint64 // defined in psci_arm64.s
