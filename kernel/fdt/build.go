package fdt

import (
	"encoding/binary"
	"errors"
)

// Builder writes a DTB (version 17). Nodes are opened with Begin and closed
// with End; properties belong to the innermost open node.
//
//	var b fdt.Builder
//	b.Begin("")                    // root
//	b.PropU32("#address-cells", 2)
//	b.Begin("memory@10000000000")
//	b.PropRegs(2, 2, fdt.Range{Base: 1 << 40, Size: 1 << 34})
//	b.End()
//	b.End()
//	blob, err := b.Bytes()
type Builder struct {
	st      []byte
	strs    []byte
	strOffs map[string]int
	depth   int
	err     error
}

func (b *Builder) u32(v uint32) { b.st = binary.BigEndian.AppendUint32(b.st, v) }

func (b *Builder) pad() {
	for len(b.st)%4 != 0 {
		b.st = append(b.st, 0)
	}
}

// Begin opens a node ("" for the root).
func (b *Builder) Begin(name string) {
	b.u32(tBeginNode)
	b.st = append(append(b.st, name...), 0)
	b.pad()
	b.depth++
}

// End closes the innermost node.
func (b *Builder) End() {
	if b.depth == 0 {
		b.err = errors.New("fdt: End without Begin")
		return
	}
	b.u32(tEndNode)
	b.depth--
}

// Prop adds a raw property.
func (b *Builder) Prop(name string, val []byte) {
	if b.depth == 0 {
		b.err = errors.New("fdt: property outside a node")
		return
	}
	if b.strOffs == nil {
		b.strOffs = map[string]int{}
	}
	off, ok := b.strOffs[name]
	if !ok {
		off = len(b.strs)
		b.strOffs[name] = off
		b.strs = append(append(b.strs, name...), 0)
	}
	b.u32(tProp)
	b.u32(uint32(len(val)))
	b.u32(uint32(off))
	b.st = append(b.st, val...)
	b.pad()
}

// PropStrings adds a string-list property (e.g. "compatible").
func (b *Builder) PropStrings(name string, s ...string) {
	var v []byte
	for _, x := range s {
		v = append(append(v, x...), 0)
	}
	b.Prop(name, v)
}

// PropU32 adds a property of 32-bit cells.
func (b *Builder) PropU32(name string, cells ...uint32) {
	var v []byte
	for _, c := range cells {
		v = binary.BigEndian.AppendUint32(v, c)
	}
	b.Prop(name, v)
}

// PropRegs adds a "reg" property with the given address and size cells.
func (b *Builder) PropRegs(addrCells, sizeCells int, regs ...Range) {
	var v []byte
	put := func(x uint64, cells int) {
		for i := cells - 1; i >= 0; i-- {
			v = binary.BigEndian.AppendUint32(v, uint32(x>>(32*i)))
		}
	}
	for _, r := range regs {
		put(r.Base, addrCells)
		put(r.Size, sizeCells)
	}
	b.Prop("reg", v)
}

// Bytes finishes the tree and returns the DTB.
func (b *Builder) Bytes() ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	if b.depth != 0 {
		return nil, errors.New("fdt: unclosed node")
	}
	const hdr, rsvmap = 40, 16 // header, one empty reservation entry
	st := binary.BigEndian.AppendUint32(append([]byte{}, b.st...), tEnd)
	offStruct := hdr + rsvmap
	offStrings := offStruct + len(st)
	total := offStrings + len(b.strs)
	out := make([]byte, hdr+rsvmap, total)
	be := binary.BigEndian
	be.PutUint32(out[0:], magic)
	be.PutUint32(out[4:], uint32(total))
	be.PutUint32(out[8:], uint32(offStruct))
	be.PutUint32(out[12:], uint32(offStrings))
	be.PutUint32(out[16:], hdr) // memory reservation map
	be.PutUint32(out[20:], 17)  // version
	be.PutUint32(out[24:], 16)  // last compatible version
	be.PutUint32(out[28:], 0)   // boot CPU
	be.PutUint32(out[32:], uint32(len(b.strs)))
	be.PutUint32(out[36:], uint32(len(st)))
	out = append(append(out, st...), b.strs...)
	return out, nil
}
