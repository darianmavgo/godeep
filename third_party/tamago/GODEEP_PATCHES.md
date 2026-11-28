# godeep patches to TamaGo v1.27.4

Vendored from `github.com/usbarmory/tamago@v1.27.4` (see `replace` in godeep's go.mod).

1. `arm64/init.go`: add `//go:build !linkhwinit0`, so a board can provide its
   own `runtime/goos.Hwinit0` (MMU setup), as the amd64 port already allows.
   godeep needs this because Apple M1 RAM starts at 1 TB, beyond the 39-bit
   address space TamaGo's arm64 MMU code maps (see godeep kernel/pgtable).
2. `arm64/mmu_state.go`: `var m mmu` moved out of init.go so `CPU.Init` still
   links when init.go is excluded.
3. `goos/mem.go`: `HeapAddrBits` 41 instead of 40 on 64-bit. With 40 bits the
   Go heap must lie below 1 TB, but Apple M1 DRAM *starts* at 1 TB, so the
   runtime aborts in `schedinit` ("memory reservation exceeds address space
   limit"). Found by running the Mac image under QEMU sbsa-ref (RAM at 1 TB)
   with `make check-applem1`. Cost: the arena index doubles to 4 MB. Boards
   with RAM low (virt, microvm) are unaffected.

4. `arm64/irq.go`, `arm64/irq.s`: an optional `IRQHandler` hook that runs
   inside the IRQ exception, on the core that took it, before the signal
   relay; with it set the exception returns with IRQs unmasked. godeep's
   `board/virt` acknowledges GIC interrupts there, so its `irq` goroutine
   can run on any core once there are several (SMP). The IRQ stack grows
   from 512 to 2048 bytes for the hook's frames.

All of them are candidates to send upstream (3 perhaps as a per-board setting).

5. `arm64/exception.go`, `exception.s`, `fault_godeep.s`, `fault_stock.s`:
   `FaultPanics`. With it set (by the board) and godeep's runtime patch
   (build tag `godeepruntime`, `runtime·tamagoFault`), a synchronous fault
   taken in a goroutine (data/instruction abort, alignment, undefined
   instruction) becomes a Go panic in that goroutine instead of a call to
   `SystemExceptionHandler`; SError and faults with no g still go there.
   See ../tamago-go/README.md.
