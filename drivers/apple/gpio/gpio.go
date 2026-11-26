// Package gpio drives Apple Silicon GPIO / pin-control blocks
// ("apple,pinctrl"), after Linux's pinctrl-apple-gpio.c and U-Boot's
// pinctrl-apple.c. Each pin has its own 32-bit register at 4*pin, so code
// using different pins never shares a register.
package gpio

import (
	"fmt"

	"godeep/kernel/fdt"
	"godeep/kernel/mmio"
)

// Per-pin register bits.
const (
	DATA         = 1 << 0 // output value; for an input, the pin level
	MODE         = 7 << 1 // see MODE_*
	PERIPH       = 3 << 5 // peripheral function (pin mux)
	INPUT_ENABLE = 1 << 9

	MODE_OUT        = 1
	MODE_IN_IRQ_OFF = 7 // input, no interrupt
)

// Block is one pin-control block.
type Block struct{ R mmio.Regs }

func reg(pin int) uint64 { return 4 * uint64(pin) }

// Set drives pin as an output.
func (b Block) Set(pin int, high bool) {
	v := b.R.Read32(reg(pin)) &^ (DATA | MODE | PERIPH)
	if high {
		v |= DATA
	}
	b.R.Write32(reg(pin), v|MODE_OUT<<1)
}

// Get returns the pin's level.
func (b Block) Get(pin int) bool { return b.R.Read32(reg(pin))&DATA != 0 }

// IsInput reports whether the pin reads its level: input enabled, not an
// output, not muxed to a peripheral.
func (b Block) IsInput(pin int) bool {
	v := b.R.Read32(reg(pin))
	return v&INPUT_ENABLE != 0 && (v&MODE)>>1 != MODE_OUT && v&PERIPH == 0
}

// Input makes pin a plain input with its interrupt off (pull settings kept).
func (b Block) Input(pin int) {
	v := b.R.Read32(reg(pin)) &^ (DATA | MODE | PERIPH)
	b.R.Write32(reg(pin), v|MODE_IN_IRQ_OFF<<1|INPUT_ENABLE)
}

// Mux routes pin to peripheral function fn. It writes only if needed and
// reports whether it did.
func (b Block) Mux(pin, fn int) bool {
	v := b.R.Read32(reg(pin))
	if int(v&PERIPH)>>5 == fn {
		return false
	}
	b.R.Write32(reg(pin), v&^PERIPH|uint32(fn)<<5&PERIPH|INPUT_ENABLE)
	return true
}

// Pin names one pin of a block.
type Pin struct {
	Block uint64 // the block's register base
	Pin   int
	Flags uint32 // binding flags (e.g. active low)
}

func (p Pin) String() string { return fmt.Sprintf("%#x/%d", p.Block, p.Pin) }

// PinFromRef converts a device-tree GPIO or interrupt reference (to an
// "apple,pinctrl" node, first argument the pin, second the flags).
func PinFromRef(r fdt.Ref) (Pin, error) {
	if !r.Node.Compatible("apple,pinctrl") {
		return Pin{}, fmt.Errorf("gpio: %s is not an apple,pinctrl block", r.Node.Path)
	}
	reg := r.Node.Reg()
	if len(reg) == 0 || len(r.Args) == 0 {
		return Pin{}, fmt.Errorf("gpio: %s: no registers or no pin number", r.Node.Path)
	}
	if n := r.Node.U32s("apple,npins"); len(n) == 1 && r.Args[0] >= n[0] {
		return Pin{}, fmt.Errorf("gpio: pin %d out of range for %s (%d pins)", r.Args[0], r.Node.Path, n[0])
	}
	p := Pin{Block: reg[0].Base, Pin: int(r.Args[0])}
	if len(r.Args) > 1 {
		p.Flags = r.Args[1]
	}
	return p, nil
}

// Mux is one pin-mux setting.
type Mux struct {
	Pin
	Func int
}

// MuxFromFDT returns the pin-mux settings of a device's "pinctrl-0" groups.
// Each group's "pinmux" cells are APPLE_PINMUX(pin, func) = pin | func<<16,
// and the group sits under its pin-control block.
func MuxFromFDT(dev *fdt.Node) ([]Mux, error) {
	groups, err := dev.Refs("pinctrl-0", "")
	if err != nil {
		return nil, err
	}
	var out []Mux
	for _, g := range groups {
		blk := g.Node.Parent
		if blk == nil || !blk.Compatible("apple,pinctrl") {
			return nil, fmt.Errorf("gpio: pin group %s is not under an apple,pinctrl block", g.Node.Path)
		}
		reg := blk.Reg()
		if len(reg) == 0 {
			return nil, fmt.Errorf("gpio: %s has no registers", blk.Path)
		}
		for _, c := range g.Node.U32s("pinmux") {
			out = append(out, Mux{Pin: Pin{Block: reg[0].Base, Pin: int(c & 0xffff)}, Func: int(c >> 16)})
		}
	}
	return out, nil
}
