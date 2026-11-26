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

All three are candidates to send upstream (3 perhaps as a per-board setting).
