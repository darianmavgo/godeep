// Package fdt parses flattened device trees (DTB), the hardware description a
// Linux-style boot loader passes in x0: QEMU when booting an Image, and m1n1
// on Apple Silicon (which converts Apple's ADT into an FDT for its payload).
package fdt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const (
	magic      = 0xd00dfeed
	tBeginNode = 1
	tEndNode   = 2
	tProp      = 3
	tNop       = 4
	tEnd       = 9
)

// Node is one device-tree node.
type Node struct {
	Name     string // e.g. "pl061@9030000"
	Path     string // e.g. "/pl061@9030000"
	Props    map[string][]byte
	Children []*Node
	Parent   *Node
}

// Parse decodes a DTB.
func Parse(b []byte) (*Node, error) {
	be := binary.BigEndian
	if len(b) < 40 || be.Uint32(b) != magic {
		return nil, errors.New("fdt: bad magic")
	}
	total := int(be.Uint32(b[4:]))
	offStruct, offStrings := int(be.Uint32(b[8:])), int(be.Uint32(b[12:]))
	sizeStrings, sizeStruct := int(be.Uint32(b[32:])), int(be.Uint32(b[36:]))
	if total > len(b) || offStruct+sizeStruct > total || offStrings+sizeStrings > total {
		return nil, errors.New("fdt: truncated")
	}
	st, strs := b[offStruct:offStruct+sizeStruct], b[offStrings:offStrings+sizeStrings]

	var root, cur *Node
	for p := 0; p+4 <= len(st); {
		tok := be.Uint32(st[p:])
		p += 4
		switch tok {
		case tBeginNode:
			end := p
			for end < len(st) && st[end] != 0 {
				end++
			}
			n := &Node{Name: string(st[p:end]), Props: map[string][]byte{}, Parent: cur}
			p = (end + 1 + 3) &^ 3
			if cur == nil {
				root, n.Path = n, "/"
			} else {
				n.Path = strings.TrimSuffix(cur.Path, "/") + "/" + n.Name
				cur.Children = append(cur.Children, n)
			}
			cur = n
		case tEndNode:
			if cur == nil {
				return nil, errors.New("fdt: unbalanced END_NODE")
			}
			cur = cur.Parent
		case tProp:
			if p+8 > len(st) || cur == nil {
				return nil, errors.New("fdt: bad property")
			}
			l, nameOff := int(be.Uint32(st[p:])), int(be.Uint32(st[p+4:]))
			p += 8
			if p+l > len(st) || nameOff >= len(strs) {
				return nil, errors.New("fdt: property out of bounds")
			}
			name := strs[nameOff:]
			if i := indexByte(name, 0); i >= 0 {
				name = name[:i]
			}
			cur.Props[string(name)] = st[p : p+l]
			p = (p + l + 3) &^ 3
		case tNop:
		case tEnd:
			if root == nil {
				return nil, errors.New("fdt: empty tree")
			}
			return root, nil
		default:
			return nil, fmt.Errorf("fdt: unknown token %#x", tok)
		}
	}
	return nil, errors.New("fdt: missing END")
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// Strings returns a string-list property (e.g. "compatible").
func (n *Node) Strings(prop string) []string {
	v, ok := n.Props[prop]
	if !ok {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(v), "\x00"), "\x00")
}

// U32s returns a property as big-endian 32-bit cells.
func (n *Node) U32s(prop string) []uint32 {
	v := n.Props[prop]
	out := make([]uint32, len(v)/4)
	for i := range out {
		out[i] = binary.BigEndian.Uint32(v[4*i:])
	}
	return out
}

func (n *Node) cells(prop string, def uint32) uint32 {
	if v := n.U32s(prop); len(v) == 1 {
		return v[0]
	}
	return def
}

// Range is an address window.
type Range struct{ Base, Size uint64 }

// Reg decodes "reg" using the parent's #address-cells/#size-cells. Addresses
// are translated through ancestors' "ranges" (empty "ranges" = identity), so
// nodes under a /soc bus (as on Apple Silicon) report CPU physical addresses.
func (n *Node) Reg() []Range {
	if n.Parent == nil {
		return nil
	}
	ac, sc := n.Parent.cells("#address-cells", 2), n.Parent.cells("#size-cells", 1)
	v := n.U32s("reg")
	var out []Range
	for i := 0; i+int(ac+sc) <= len(v); i += int(ac + sc) {
		r := Range{Base: join(v[i : i+int(ac)]), Size: join(v[i+int(ac) : i+int(ac+sc)])}
		r.Base = n.Parent.translate(r.Base)
		out = append(out, r)
	}
	return out
}

// translate maps a child bus address through this node's "ranges" and up.
func (n *Node) translate(addr uint64) uint64 {
	if n.Parent == nil {
		return addr
	}
	raw, ok := n.Props["ranges"]
	if !ok || len(raw) == 0 {
		return n.Parent.translate(addr)
	}
	cac, pac, sc := n.cells("#address-cells", 2), n.Parent.cells("#address-cells", 2), n.cells("#size-cells", 1)
	v, step := n.U32s("ranges"), int(cac+pac+sc)
	for i := 0; i+step <= len(v); i += step {
		child := join(v[i : i+int(cac)])
		parent := join(v[i+int(cac) : i+int(cac+pac)])
		size := join(v[i+int(cac+pac) : i+step])
		if addr >= child && addr < child+size {
			return n.Parent.translate(addr - child + parent)
		}
	}
	return n.Parent.translate(addr)
}

func join(c []uint32) uint64 {
	var x uint64
	for _, w := range c {
		x = x<<32 | uint64(w)
	}
	return x
}

// Walk visits every node depth-first.
func (n *Node) Walk(fn func(*Node)) {
	fn(n)
	for _, c := range n.Children {
		c.Walk(fn)
	}
}

// Find returns the node at path ("/chosen") or nil.
func (n *Node) Find(path string) *Node {
	var found *Node
	n.Walk(func(x *Node) {
		if found == nil && x.Path == path {
			found = x
		}
	})
	return found
}

// GICInterrupts decodes an "interrupts" property in the ARM GIC binding
// (3 cells: type, number, flags) into GIC interrupt IDs: SPI n -> 32+n,
// PPI n -> 16+n.
func (n *Node) GICInterrupts() []int {
	v := n.U32s("interrupts")
	var out []int
	for i := 0; i+3 <= len(v); i += 3 {
		switch v[i] {
		case 0:
			out = append(out, 32+int(v[i+1]))
		case 1:
			out = append(out, 16+int(v[i+1]))
		}
	}
	return out
}

// Phandle returns the node's phandle (0 if it has none).
func (n *Node) Phandle() uint32 {
	for _, p := range []string{"phandle", "linux,phandle"} {
		if v := n.U32s(p); len(v) == 1 {
			return v[0]
		}
	}
	return 0
}

// Root returns the root of the tree n belongs to.
func (n *Node) Root() *Node {
	for n.Parent != nil {
		n = n.Parent
	}
	return n
}

// ByPhandle finds the node with phandle h in n's tree, or nil.
func (n *Node) ByPhandle(h uint32) *Node {
	if h == 0 {
		return nil
	}
	var found *Node
	n.Root().Walk(func(x *Node) {
		if found == nil && x.Phandle() == h {
			found = x
		}
	})
	return found
}

// Ref is one entry of a phandle list: the node pointed to and its argument
// cells (e.g. a GPIO number and flags).
type Ref struct {
	Node *Node
	Args []uint32
}

// Refs decodes a phandle-with-arguments list such as "spien-gpios",
// "interrupts-extended" or "power-domains". The number of argument cells
// comes from the target's cellsProp ("#gpio-cells", "#interrupt-cells",
// "#power-domain-cells"); a target without it has none.
func (n *Node) Refs(prop, cellsProp string) ([]Ref, error) {
	v := n.U32s(prop)
	var out []Ref
	for i := 0; i < len(v); {
		t := n.ByPhandle(v[i])
		if t == nil {
			return nil, fmt.Errorf("fdt: %s: %s: no node with phandle %#x", n.Path, prop, v[i])
		}
		c := int(t.cells(cellsProp, 0))
		if i+1+c > len(v) {
			return nil, fmt.Errorf("fdt: %s: %s: truncated (%s needs %d cells)", n.Path, prop, t.Path, c)
		}
		out = append(out, Ref{Node: t, Args: v[i+1 : i+1+c]})
		i += 1 + c
	}
	return out, nil
}

// Compatible reports whether the node lists compat in "compatible".
func (n *Node) Compatible(compat string) bool {
	for _, c := range n.Strings("compatible") {
		if c == compat {
			return true
		}
	}
	return false
}
