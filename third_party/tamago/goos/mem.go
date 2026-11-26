// Custom GOOS support
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

//go:build !tiny

package goos

const bits = 32 << (^uint(0) >> 63) / 8

// Required constants.
const (
	// ArenaBaseOffset is the pointer value that corresponds to index 0 in
	// the heap arena map (see runtime.arenaBaseOffset).
	ArenaBaseOffset = 0

	// HeapAddrBits defines the number of bits in a heap address (see
	// runtime.heapAddrBits).
	//
	// godeep: 41 on 64-bit (upstream: 40). Apple M1 DRAM starts at 1 TB
	// (1<<40), so a heap there needs 41 bits; with 40 the runtime aborts in
	// schedinit ("memory reservation exceeds address space limit"). Costs a
	// 4 MB arena index instead of 2 MB. See GODEEP_PATCHES.md.
	HeapAddrBits = (8-bits)*3 + bits*5 + bits/8 // 32-bit:32 64-bit:41

	// LogHeapArenaBytes defines the size of a runtime heap arena in log_2
	// bytes (see runtime.logHeapArenaBytes).
	LogHeapArenaBytes = (2 + 20)

	// LogPallocChunkPages defines the size of a runtime bitmap chunk in
	// log_2 bytes (see runtime.logPallocChunkPages).
	LogPallocChunkPages = 9

	// MinPhysPageSize is a lower-bound on the physical page size (see
	// runtime.minPhysPageSize).
	MinPhysPageSize = 4096

	// StackSystem is a number of additional bytes to add to each stack
	// below the usual guard area.
	StackSystem = 512
)
