package fdt

import "encoding/binary"

// EarlyFB describes a "simple-framebuffer" node.
type EarlyFB struct {
	Base, Size            uint64
	Width, Height, Stride uint32
	Format                []byte // e.g. "x2r10g10b10" (a slice of the blob, no NUL)
}

// Early is what a board needs from the device tree before the Go runtime
// (and its heap) exists: where to draw, and optionally where to print.
type Early struct {
	FB       EarlyFB
	HasFB    bool
	UART     uint64 // base of the first "arm,pl011" node, 0 if none
	Bootargs []byte // /chosen/bootargs (a slice of the blob, no NUL), nil if none

	// For boards that take the interrupt controller, RTC and PSCI from
	// the tree (board/virt: QEMU virt, Parallels Desktop).
	GICD, GICR uint64 // first "arm,gic-v3": distributor and redistributor region, 0 if none
	RTC        uint64 // base of the first "arm,pl031", 0 if none
	PSCI       []byte // "method" ("hvc", "smc") of the first "arm,psci*" node, nil if none
}

const earlyMaxDepth = 16

// earlyNode collects the properties ScanEarly needs from one node.
type earlyNode struct {
	addrCells, sizeCells  uint32 // for this node's children
	compat, reg, format   []byte
	method                []byte
	width, height, stride []byte
	chosen                bool // this is /chosen
	done                  bool
}

// ScanEarly finds the first simple-framebuffer and PL011 in a DTB without
// allocating, so it can run in Hwinit0. Addresses are taken as-is: like
// m1n1's /chosen ("ranges;"), parents must map them 1:1. A malformed blob
// gives an empty result, never a panic.
func ScanEarly(b []byte) (e Early) {
	be := binary.BigEndian
	if len(b) < 40 || be.Uint32(b) != magic {
		return
	}
	total := int(be.Uint32(b[4:]))
	offStruct, offStrings := int(be.Uint32(b[8:])), int(be.Uint32(b[12:]))
	sizeStrings, sizeStruct := int(be.Uint32(b[32:])), int(be.Uint32(b[36:]))
	if total > len(b) || offStruct < 0 || offStrings < 0 || sizeStruct < 0 || sizeStrings < 0 ||
		offStruct+sizeStruct > total || offStrings+sizeStrings > total {
		return
	}
	st, strs := b[offStruct:offStruct+sizeStruct], b[offStrings:offStrings+sizeStrings]

	var stack [earlyMaxDepth + 1]earlyNode
	depth := -1 // index of the innermost open node
	parentCells := func() (uint32, uint32) {
		if depth <= 0 {
			return 2, 1 // spec defaults for the root's children... and the root itself
		}
		p := &stack[depth-1]
		return p.addrCells, p.sizeCells
	}
	// finish evaluates the innermost node once its properties are all seen
	// (properties come before subnodes in a DTB).
	finish := func() {
		if depth < 0 || stack[depth].done {
			return
		}
		n := &stack[depth]
		n.done = true
		ac, sc := parentCells()
		if !e.HasFB && hasString(n.compat, "simple-framebuffer") {
			base, size, ok := firstReg(n.reg, ac, sc)
			if ok && len(n.width) == 4 && len(n.height) == 4 && len(n.stride) == 4 && len(n.format) > 0 {
				e.FB = EarlyFB{Base: base, Size: size,
					Width: be.Uint32(n.width), Height: be.Uint32(n.height), Stride: be.Uint32(n.stride),
					Format: trimNUL(n.format)}
				e.HasFB = true
			}
		}
		if e.UART == 0 && hasString(n.compat, "arm,pl011") {
			if base, _, ok := firstReg(n.reg, ac, sc); ok {
				e.UART = base
			}
		}
		if e.GICD == 0 && hasString(n.compat, "arm,gic-v3") {
			d, _, ok1 := nthReg(n.reg, ac, sc, 0)
			r, _, ok2 := nthReg(n.reg, ac, sc, 1)
			if ok1 && ok2 {
				e.GICD, e.GICR = d, r
			}
		}
		if e.RTC == 0 && hasString(n.compat, "arm,pl031") {
			if base, _, ok := firstReg(n.reg, ac, sc); ok {
				e.RTC = base
			}
		}
		if e.PSCI == nil && len(n.method) > 0 && (hasString(n.compat, "arm,psci") ||
			hasString(n.compat, "arm,psci-0.2") || hasString(n.compat, "arm,psci-1.0")) {
			e.PSCI = trimNUL(n.method)
		}
	}

	for p := 0; p+4 <= len(st); {
		tok := be.Uint32(st[p:])
		p += 4
		switch tok {
		case tBeginNode:
			finish() // the parent's properties are complete
			end := p
			for end < len(st) && st[end] != 0 {
				end++
			}
			name := st[p:end]
			p = (end + 1 + 3) &^ 3
			if depth+1 > earlyMaxDepth {
				return
			}
			depth++
			stack[depth] = earlyNode{addrCells: 2, sizeCells: 1, chosen: depth == 1 && eq(name, "chosen")}
		case tEndNode:
			finish()
			if depth < 0 {
				return
			}
			depth--
		case tProp:
			if p+8 > len(st) || depth < 0 {
				return
			}
			l, nameOff := int(be.Uint32(st[p:])), int(be.Uint32(st[p+4:]))
			p += 8
			if l < 0 || p+l > len(st) || nameOff < 0 || nameOff >= len(strs) {
				return
			}
			val := st[p : p+l]
			p = (p + l + 3) &^ 3
			n := &stack[depth]
			switch name := cstr(strs[nameOff:]); {
			case eq(name, "#address-cells") && l == 4:
				n.addrCells = be.Uint32(val)
			case eq(name, "#size-cells") && l == 4:
				n.sizeCells = be.Uint32(val)
			case eq(name, "compatible"):
				n.compat = val
			case eq(name, "reg"):
				n.reg = val
			case eq(name, "format"):
				n.format = val
			case eq(name, "method"):
				n.method = val
			case eq(name, "width"):
				n.width = val
			case eq(name, "height"):
				n.height = val
			case eq(name, "stride"):
				n.stride = val
			case eq(name, "bootargs") && n.chosen:
				e.Bootargs = trimNUL(val)
			}
		case tNop:
		case tEnd:
			return
		default:
			return
		}
	}
	return
}

// cstr returns b up to its first NUL.
func cstr(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

func trimNUL(b []byte) []byte { return cstr(b) }

func eq(b []byte, s string) bool { return string(b) == s }

// hasString reports whether a NUL-separated string list contains s.
func hasString(list []byte, s string) bool {
	for len(list) > 0 {
		x := cstr(list)
		if eq(x, s) {
			return true
		}
		if len(x) == len(list) {
			break
		}
		list = list[len(x)+1:]
	}
	return false
}

// firstReg decodes the first (address, size) pair of a reg property.
func firstReg(reg []byte, ac, sc uint32) (base, size uint64, ok bool) {
	return nthReg(reg, ac, sc, 0)
}

// nthReg decodes the (address, size) pair at index i of a reg property.
func nthReg(reg []byte, ac, sc uint32, i int) (base, size uint64, ok bool) {
	if ac == 0 || ac > 2 || sc > 2 || i < 0 || len(reg) < (i+1)*int(ac+sc)*4 {
		return 0, 0, false
	}
	reg = reg[i*int(ac+sc)*4:]
	be := binary.BigEndian
	for i := uint32(0); i < ac; i++ {
		base = base<<32 | uint64(be.Uint32(reg[i*4:]))
	}
	for i := uint32(0); i < sc; i++ {
		size = size<<32 | uint64(be.Uint32(reg[(ac+i)*4:]))
	}
	return base, size, true
}
