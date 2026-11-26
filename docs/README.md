# godeep Documentation Index

Start with [`README.md`](../README.md) for the hands-on tutorial, or [`GEMINI.md`](../GEMINI.md) / [`CLAUDE.md`](../CLAUDE.md) for architecture rules, build commands, and platform constraints.

## Guides & References

| Document | Status | What it covers |
|---|---|---|
| [`DriverModel.md`](DriverModel.md) | Current | Driver rules, `kernel/dma`, `kernel/irq`, `kernel/dev`, `kernel/rtkit`, portable `drivers/*`, and hardware bugs encountered |
| [`TargetMyMac.md`](TargetMyMac.md) | Active roadmap | Apple M1 Pro (`J316sAP` / `t6000`) hardware inventory, Go driver coverage, native boot status, two-machine vs. QEMU split, and milestones |
| [`PartitioningMac.md`](PartitioningMac.md) | Current runbook | Carving out a `godeep` boot entry + `GODEEPDATA` partition next to macOS and Omarchy, `make native`, QEMU `sbsa-ref` rehearsal, and early boot/fault diagnostics |
| [`Using2Computers.md`](Using2Computers.md) | Active workflow | Tethered two-machine setup (Windows 11/WSL2 dev host + Mac target via m1n1 proxy/hypervisor and `tools/setup-windows.ps1`) |
| [`SSHServer.md`](SSHServer.md) | Active / mostly built | `programs/sshd` (`x/crypto/ssh` + SFTP v3), networking, WASM loading, 9P, and the planned Mac serial-SSH bridge |
| [`architecture.md`](architecture.md) | Status + historical | Current phase progress summary at the top, followed by the original Phase-0 bootstrap proposal |
| [`ReUsingGVisor.md`](ReUsingGVisor.md) | Historical design | Analysis of gVisor reuse (`pkg/tcpip` vs. Sentry) and capability equivalents for Linux syscalls (with current-status note) |
| [`XNUPort.md`](XNUPort.md) | Assessment | Analysis of what would change if godeep had started as an Apple XNU-to-Go port (what XNU lacks for M1 drivers vs. what's worth borrowing) |
| [`GoGpuSupport.md`](GoGpuSupport.md) | Historical + roadmap | Display and compute options (`ramfb` built, `virtio-gpu` 2D, CPU SIMD + goroutines, host GPU offload) |
| [`../third_party/tamago/GODEEP_PATCHES.md`](../third_party/tamago/GODEEP_PATCHES.md) | Current | Rationale for the 3 vendored TamaGo v1.27.4 patches (`linkhwinit0` and `HeapAddrBits=41`) |

## Logs & Standalone HTML References

| File | Type | Description |
|---|---|---|
| [`TargetMyMacProgress.log`](TargetMyMacProgress.log) | Append-only log | Chronological native Mac bring-up log. **Never edit, rename, or delete** — append only via `tools/progress-log.sh` (enforced by `tools/githooks/pre-commit`) |
| [`godeep_on_apple_m1_pro_targetmymac_md_reference.html`](godeep_on_apple_m1_pro_targetmymac_md_reference.html) | Reference HTML | Interactive visual reference & acronym glossary for `TargetMyMac.md` on M1 Pro |
| [`storage-safety-layers.html`](storage-safety-layers.html) | Reference HTML | Comparison of storage safety layers across OS and runtime stacks |
