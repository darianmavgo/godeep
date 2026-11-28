// Command agxlayout computes the memory layout of the Apple GPU firmware's
// structures from Asahi Linux's drm/asahi source (drivers/gpu/drm/asahi/
// fw/*.rs, "GPL-2.0-only OR MIT"), for given GPU generations and firmware
// versions, and writes them as Go tables for drivers/apple/agx/fw.
//
//	go run ./tools/agxlayout -src PATH/drivers/gpu/drm/asahi/fw -out drivers/apple/agx/fw/layout_gen.go
//
// The structures are #[repr(C)] (or packed); fields carry #[ver(...)]
// conditions on the firmware version V and GPU generation G. Most fields
// are named after their offset in the oldest layout (unk_1b4, pad_644);
// the tool checks those names against the computed offsets for G13 12.3
// and fails if they disagree, so a parsing or alignment mistake can't go
// unnoticed.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Firmware versions and GPU generations, in order (drm/asahi's ver macro).
var versions = []string{"V12_3", "V12_4", "V13_0B4", "V13_2", "V13_3", "V13_5"}
var gens = []string{"G13", "G14", "G14X"}

// Target is one (generation, firmware) combination.
type Target struct{ G, V string }

func (t Target) Name() string { return t.G + strings.TrimPrefix(t.V, "V") }

type field struct {
	name, typ, cond string
}

type def struct {
	mod, name string
	fields    []field
	packed    bool
	tuple     bool // struct X(T);
}

type constDef struct {
	mod, name string
	plain     string      // = 0x34;
	alts      [][2]string // {cond, value}
}

type program struct {
	structs map[string]*def // mod.Name
	byName  map[string][]*def
	consts  map[string]*constDef
	aliases map[string]string // mod.Name → type
	enums   map[string]bool   // mod.Name: repr(u32) unit enums
}

var (
	reStruct = regexp.MustCompile(`^\s*(?:pub(?:\(crate\))?\s+)?struct\s+([A-Za-z0-9_]+)\s*(<[^{]*>)?\s*\{\s*$`)
	reTuple  = regexp.MustCompile(`^\s*(?:pub(?:\(crate\))?\s+)?struct\s+([A-Za-z0-9_]+)\s*(?:<[^(]*>)?\((?:pub(?:\(crate\))?\s+)?([^;]*)\);`)
	reField  = regexp.MustCompile(`^\s*(?:pub(?:\(crate\))?\s+)?([a-z_][a-z0-9_]*)\s*:\s*(.+?),?\s*(?://.*)?$`)
	reVer    = regexp.MustCompile(`^\s*#\[ver\((.*)\)\]\s*$`)
	reRepr   = regexp.MustCompile(`^\s*#\[repr\((.*)\)\]\s*$`)
	reConst  = regexp.MustCompile(`^\s*(?:pub(?:\(crate\))?\s+)?const\s+([A-Z0-9_]+)\s*:\s*(?:usize|u32)\s*=\s*(.*)$`)
	reAlias  = regexp.MustCompile(`^\s*(?:pub(?:\(crate\))?\s+)?type\s+([A-Za-z0-9_]+)\s*=\s*(.+);`)
	reEnum   = regexp.MustCompile(`^\s*(?:pub(?:\(crate\))?\s+)?enum\s+([A-Za-z0-9_]+)\s*\{`)
)

func parse(prog *program, mod string, lines []string) error {
	var repr, ver string
	for i := 0; i < len(lines); i++ {
		l := strings.TrimSpace(strings.SplitN(lines[i], "//", 2)[0])
		if l == "" {
			continue
		}
		if m := reRepr.FindStringSubmatch(lines[i]); m != nil {
			repr = m[1]
			continue
		}
		if m := reVer.FindStringSubmatch(lines[i]); m != nil {
			ver = m[1]
			continue
		}
		if strings.HasPrefix(l, "#[") {
			continue
		}
		if m := reEnum.FindStringSubmatch(lines[i]); m != nil {
			if repr == "u32" {
				prog.enums[mod+"."+m[1]] = true
			}
			repr, ver = "", ""
			continue
		}
		if m := reConst.FindStringSubmatch(lines[i]); m != nil {
			c := &constDef{mod: mod, name: m[1]}
			rest := strings.TrimSpace(m[2])
			if rest != "{" {
				c.plain = strings.TrimSuffix(rest, ";")
			} else {
				cond := ""
				for i++; i < len(lines); i++ {
					t := strings.TrimSpace(lines[i])
					if t == "};" {
						break
					}
					if mv := reVer.FindStringSubmatch(lines[i]); mv != nil {
						cond = mv[1]
					} else if t != "{" && t != "}" && t != "" {
						c.alts = append(c.alts, [2]string{cond, t})
					}
				}
			}
			prog.consts[mod+"."+c.name] = c
			repr, ver = "", ""
			continue
		}
		if m := reAlias.FindStringSubmatch(lines[i]); m != nil {
			prog.aliases[mod+"."+m[1]] = m[2]
			repr, ver = "", ""
			continue
		}
		if strings.Contains(l, "struct ") && strings.HasSuffix(l, "(") && repr != "" {
			// struct X(\n a,\n b,\n);
			for i+1 < len(lines) && !strings.HasSuffix(l, ");") {
				i++
				l += " " + strings.TrimSpace(strings.SplitN(lines[i], "//", 2)[0])
			}
		}
		if m := reTuple.FindStringSubmatch(l); m != nil && repr != "" {
			d := &def{mod: mod, name: m[1], tuple: true, packed: strings.Contains(repr, "packed")}
			for j, t := range splitTop(m[2]) {
				t = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "pub(crate)"))
				if t != "" {
					d.fields = append(d.fields, field{name: strconv.Itoa(j), typ: t})
				}
			}
			add(prog, d)
			repr, ver = "", ""
			continue
		}
		if m := reStruct.FindStringSubmatch(lines[i]); m != nil {
			d := &def{mod: mod, name: m[1], packed: strings.Contains(repr, "packed")}
			isC := repr != ""
			fver := ""
			for i++; i < len(lines); i++ {
				raw := lines[i]
				t := strings.TrimSpace(strings.SplitN(raw, "//", 2)[0])
				if t == "}" {
					break
				}
				if mv := reVer.FindStringSubmatch(raw); mv != nil {
					fver = mv[1]
					continue
				}
				if t == "" || strings.HasPrefix(t, "#[") {
					continue
				}
				// A type may continue on the next lines.
				for (strings.HasSuffix(t, ":") || unbalanced(t)) && i+1 < len(lines) {
					i++
					t += " " + strings.TrimSpace(strings.SplitN(lines[i], "//", 2)[0])
				}
				mf := reField.FindStringSubmatch(t)
				if mf == nil {
					return fmt.Errorf("%s:%d: can't parse field %q", mod, i+1, t)
				}
				d.fields = append(d.fields, field{name: mf[1], typ: strings.TrimSuffix(strings.TrimSpace(mf[2]), ","), cond: fver})
				fver = ""
			}
			if isC {
				add(prog, d)
			}
			repr, ver = "", ""
			continue
		}
		repr, ver = "", ""
	}
	_ = ver
	return nil
}

func unbalanced(s string) bool {
	d := 0
	for _, c := range s {
		switch c {
		case '<', '[', '(':
			d++
		case '>', ']', ')':
			d--
		}
	}
	return d > 0
}

func add(prog *program, d *def) {
	prog.structs[d.mod+"."+d.name] = d
	prog.byName[d.name] = append(prog.byName[d.name], d)
}

// eval evaluates a #[ver(...)] condition for t.
func eval(cond string, t Target) (bool, error) {
	p := &condParser{s: strings.ReplaceAll(cond, " ", ""), t: t}
	v, err := p.or()
	if err == nil && p.i != len(p.s) {
		err = fmt.Errorf("trailing %q", p.s[p.i:])
	}
	return v, err
}

type condParser struct {
	s string
	i int
	t Target
}

func (p *condParser) or() (bool, error) {
	v, err := p.and()
	for err == nil && strings.HasPrefix(p.s[p.i:], "||") {
		p.i += 2
		var w bool
		w, err = p.and()
		v = v || w
	}
	return v, err
}

func (p *condParser) and() (bool, error) {
	v, err := p.atom()
	for err == nil && strings.HasPrefix(p.s[p.i:], "&&") {
		p.i += 2
		var w bool
		w, err = p.atom()
		v = v && w
	}
	return v, err
}

func (p *condParser) atom() (bool, error) {
	if p.i < len(p.s) && p.s[p.i] == '(' {
		p.i++
		v, err := p.or()
		if err != nil {
			return false, err
		}
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return false, fmt.Errorf("missing ) in %q", p.s)
		}
		p.i++
		return v, nil
	}
	if p.i >= len(p.s) {
		return false, fmt.Errorf("truncated %q", p.s)
	}
	which := p.s[p.i]
	p.i++
	var op string
	for _, o := range []string{">=", "<=", "==", "!=", ">", "<"} {
		if strings.HasPrefix(p.s[p.i:], o) {
			op = o
			break
		}
	}
	if op == "" {
		return false, fmt.Errorf("no operator in %q", p.s)
	}
	p.i += len(op)
	j := p.i
	for j < len(p.s) && (p.s[j] == '_' || p.s[j] >= '0' && p.s[j] <= '9' || p.s[j] >= 'A' && p.s[j] <= 'Z') {
		j++
	}
	name := p.s[p.i:j]
	p.i = j
	var list []string
	var cur string
	switch which {
	case 'V':
		list, cur = versions, p.t.V
	case 'G':
		list, cur = gens, p.t.G
	default:
		return false, fmt.Errorf("unknown variable %c in %q", which, p.s)
	}
	a, b := indexOf(list, cur), indexOf(list, name)
	if b < 0 {
		return false, fmt.Errorf("unknown %c value %q", which, name)
	}
	switch op {
	case ">=":
		return a >= b, nil
	case "<=":
		return a <= b, nil
	case "==":
		return a == b, nil
	case "!=":
		return a != b, nil
	case ">":
		return a > b, nil
	}
	return a < b, nil
}

func indexOf(l []string, s string) int {
	for i, x := range l {
		if x == s {
			return i
		}
	}
	return -1
}

// Field is one laid-out field.
type Field struct {
	Name      string
	Off, Size int
	Type      string
}

// Layout is a laid-out structure.
type Layout struct {
	Size, Align int
	Fields      []Field
}

type layouter struct {
	prog  *program
	t     Target
	cache map[string]*Layout
	busy  map[string]bool
}

func (l *layouter) constVal(mod, name string) (int, error) {
	c := l.prog.consts[mod+"."+name]
	if c == nil {
		for k, v := range l.prog.consts {
			if strings.HasSuffix(k, "."+name) {
				c = v
			}
		}
	}
	if c == nil {
		return 0, fmt.Errorf("unknown constant %s", name)
	}
	if c.plain != "" {
		return num(c.plain)
	}
	for _, a := range c.alts {
		ok, err := eval(a[0], l.t)
		if err != nil {
			return 0, err
		}
		if ok {
			return num(a[1])
		}
	}
	return 0, fmt.Errorf("constant %s has no value for %s", name, l.t.Name())
}

func num(s string) (int, error) {
	s = strings.TrimSpace(strings.TrimSuffix(s, "usize"))
	v, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 0, 64)
	return int(v), err
}

// splitTop splits s at top-level commas (outside <>, [], ()).
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, c := range s {
		switch c {
		case '<', '[', '(':
			depth++
		case '>', ']', ')':
			depth--
		case ',', ';':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

func (l *layouter) size(mod, typ string) (size, align int, err error) {
	typ = strings.TrimSpace(typ)
	typ = strings.ReplaceAll(typ, "'a, ", "")
	typ = strings.ReplaceAll(typ, "<'a>", "")
	typ = strings.TrimSuffix(typ, "::ver")
	switch typ {
	case "u8", "i8", "bool":
		return 1, 1, nil
	case "u16", "i16":
		return 2, 2, nil
	case "u32", "i32", "F32", "AtomicU32", "AtomicI32", "Stamp", "FwStamp", "EventValue", "OpHeader":
		return 4, 4, nil
	case "u64", "i64", "usize", "AtomicU64":
		return 8, 8, nil
	case "U32":
		return 4, 1, nil
	case "U64", "GpuRawPointer":
		return 8, 1, nil
	}
	if strings.HasPrefix(typ, "PhantomData") {
		return 0, 1, nil
	}
	if strings.HasPrefix(typ, "op::") { // microseq simple_op!: an OpHeader (u32)
		return 4, 4, nil
	}
	// [T; N]
	if strings.HasPrefix(typ, "[") && strings.HasSuffix(typ, "]") {
		parts := splitTop(typ[1 : len(typ)-1])
		if len(parts) == 2 {
			n, err := l.count(mod, parts[1])
			if err != nil {
				return 0, 0, err
			}
			s, a, err := l.size(mod, parts[0])
			return s * n, a, err
		}
	}
	base, args := typ, ""
	if i := strings.IndexByte(typ, '<'); i > 0 && strings.HasSuffix(typ, ">") {
		base, args = typ[:i], typ[i+1:len(typ)-1]
	}
	base = strings.TrimSuffix(base, "::ver")
	switch base {
	case "GpuPointer", "GpuWeakPointer", "GpuRawPointer":
		return 8, 1, nil
	case "Option":
		inner := strings.TrimSpace(args)
		if strings.HasPrefix(inner, "GpuPointer") || strings.HasPrefix(inner, "GpuWeakPointer") {
			return 8, 1, nil
		}
		return 0, 0, fmt.Errorf("Option<%s>", inner)
	case "Pad":
		n, err := l.count(mod, args)
		return n, 1, err
	case "Array":
		parts := splitTop(args)
		if len(parts) != 2 {
			return 0, 0, fmt.Errorf("bad Array %q", typ)
		}
		n, err := l.count(mod, parts[0])
		if err != nil {
			return 0, 0, err
		}
		s, a, err := l.size(mod, parts[1])
		return s * n, a, err
	}
	// A named struct, enum or alias, maybe with a module path.
	path := strings.Split(base, "::")
	name := path[len(path)-1]
	tmod := mod
	for _, p := range path[:len(path)-1] {
		if p != "raw" && p != "super" && p != "fw" && p != "crate" {
			tmod = p
		}
	}
	key := tmod + "." + name
	if l.prog.enums[key] {
		return 4, 4, nil
	}
	if a, ok := l.prog.aliases[key]; ok {
		return l.size(tmod, a)
	}
	d := l.prog.structs[key]
	if d == nil {
		if c := l.prog.byName[name]; len(c) == 1 {
			d = c[0]
		} else if l.prog.enums[findEnum(l.prog, name)] {
			return 4, 4, nil
		} else {
			return 0, 0, fmt.Errorf("unknown type %q (in %s)", typ, mod)
		}
	}
	lay, err := l.layout(d)
	if err != nil {
		return 0, 0, err
	}
	return lay.Size, lay.Align, nil
}

func findEnum(prog *program, name string) string {
	for k := range prog.enums {
		if strings.HasSuffix(k, "."+name) {
			return k
		}
	}
	return ""
}

func (l *layouter) count(mod, s string) (int, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "{"))
	s = strings.TrimSpace(strings.TrimSuffix(s, "}"))
	if n, err := num(s); err == nil {
		return n, nil
	}
	// CONST, CONST::ver, a * b
	if a, b, ok := strings.Cut(s, "*"); ok {
		x, err := l.count(mod, a)
		if err != nil {
			return 0, err
		}
		y, err := l.count(mod, b)
		return x * y, err
	}
	if a, b, ok := strings.Cut(s, "+"); ok {
		x, err := l.count(mod, a)
		if err != nil {
			return 0, err
		}
		y, err := l.count(mod, b)
		return x + y, err
	}
	name := strings.TrimSuffix(s, "::ver")
	if i := strings.LastIndex(name, "::"); i >= 0 {
		name = name[i+2:]
	}
	return l.constVal(mod, name)
}

func (l *layouter) layout(d *def) (*Layout, error) {
	key := d.mod + "." + d.name
	if lay := l.cache[key]; lay != nil {
		return lay, nil
	}
	if l.busy[key] {
		return nil, fmt.Errorf("recursive type %s", key)
	}
	l.busy[key] = true
	defer delete(l.busy, key)
	lay := &Layout{Align: 1}
	off := 0
	for _, f := range d.fields {
		if f.cond != "" {
			ok, err := eval(f.cond, l.t)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %v", key, f.name, err)
			}
			if !ok {
				continue
			}
		}
		s, a, err := l.size(d.mod, f.typ)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %v", key, f.name, err)
		}
		if d.packed {
			a = 1
		}
		off = alignUp(off, a)
		lay.Fields = append(lay.Fields, Field{Name: f.name, Off: off, Size: s, Type: f.typ})
		off += s
		lay.Align = max(lay.Align, a)
	}
	lay.Size = alignUp(off, lay.Align)
	l.cache[key] = lay
	return lay, nil
}

func alignUp(x, a int) int { return (x + a - 1) / a * a }

// knownMismatch lists fields whose names aren't their 12.3 offsets, each
// checked by hand (2026-11-27): UatLevelInfo numbers its fields; InitData's
// unk_8/unk_c are at 8/c (its other names count the 13.x ver_info);
// RunVertex and JobParameters2 follow a newer layout's offsets;
// Globals.unk_117bc follows 0x100 eight-byte PendingStamps (as in Rust).
var knownMismatch = map[string]bool{
	"initdata.Globals.unk_117bc": true, "initdata.InitData.unk_8": true, "initdata.InitData.unk_c": true,
	"initdata.UatLevelInfo.unk_3": true, "initdata.UatLevelInfo.unk_4": true, "vertex.JobParameters2.unk_480": true,
	"vertex.RunVertex.unk_1c": true, "vertex.RunVertex.unk_55c": true, "vertex.RunVertex.unk_560": true,
	"vertex.RunVertex.unk_568": true,
}

var reOffName = regexp.MustCompile(`^(?:unk|pad)_([0-9a-f]+)$`)

// check compares offset-named fields with their computed offsets. Names
// are offsets in the oldest layout, either within the struct or within
// the struct that embeds it: so one constant base per struct is allowed
// (the most common name - offset difference), and only fields that break
// it are reported.
func check(lays map[string]*Layout) (bad []string, checked int) {
	for k, lay := range lays {
		type nf struct {
			f    Field
			want int
		}
		var named []nf
		count := map[int]int{}
		for _, f := range lay.Fields {
			m := reOffName.FindStringSubmatch(f.Name)
			if m == nil {
				continue
			}
			want, _ := strconv.ParseInt(m[1], 16, 64)
			named = append(named, nf{f, int(want)})
			count[int(want)-f.Off]++
		}
		base, best := 0, -1
		for d, n := range count {
			if n > best || n == best && d < base {
				base, best = d, n
			}
		}
		for _, x := range named {
			checked++
			if x.want-x.f.Off != base {
				bad = append(bad, fmt.Sprintf("%s.%s at %#x (base %#x, name says %#x)", k, x.f.Name, x.f.Off, base, x.want-base))
			}
		}
	}
	sort.Strings(bad)
	return bad, checked
}

func main() {
	src := flag.String("src", "", "drm/asahi's fw directory")
	out := flag.String("out", "", "Go file to write")
	pkg := flag.String("pkg", "fw", "Go package name")
	flag.Parse()
	if *src == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	prog := &program{structs: map[string]*def{}, byName: map[string][]*def{}, consts: map[string]*constDef{},
		aliases: map[string]string{}, enums: map[string]bool{}}
	files, _ := filepath.Glob(filepath.Join(*src, "*.rs"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			fatal(err)
		}
		if err := parse(prog, strings.TrimSuffix(filepath.Base(f), ".rs"), strings.Split(string(b), "\n")); err != nil {
			fatal(err)
		}
	}
	targets := []Target{{"G13", "V12_3"}, {"G13", "V13_5"}}
	all := map[Target]map[string]*Layout{}
	var keys []string
	for k := range prog.structs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	skipped := map[string]string{}
	for _, t := range targets {
		l := &layouter{prog: prog, t: t, cache: map[string]*Layout{}, busy: map[string]bool{}}
		lays := map[string]*Layout{}
		for _, k := range keys {
			lay, err := l.layout(prog.structs[k])
			if err != nil {
				skipped[k] = err.Error()
				continue
			}
			lays[k] = lay
		}
		all[t] = lays
	}
	bad, checked := check(all[targets[0]])
	fmt.Fprintf(os.Stderr, "agxlayout: %d structures, %d offset names checked, %d mismatches, %d skipped\n",
		len(all[targets[0]]), checked, len(bad), len(skipped))
	for _, b := range bad {
		fmt.Fprintln(os.Stderr, "  mismatch:", b)
	}
	for k, e := range skipped {
		fmt.Fprintf(os.Stderr, "  skipped %s: %s\n", k, e)
	}
	var unexpected []string
	for _, b := range bad {
		if !knownMismatch[strings.SplitN(b, " ", 2)[0]] {
			unexpected = append(unexpected, b)
		}
	}
	if len(unexpected) > 0 {
		fatal(fmt.Errorf("unexpected offset-name mismatches: %v", unexpected))
	}

	bw := &bytes.Buffer{}
	fmt.Fprintf(bw, "// Code generated by tools/agxlayout from drm/asahi fw/*.rs (GPL-2.0-only OR MIT, used under MIT); DO NOT EDIT.\n\npackage %s\n\n", *pkg)
	for _, t := range targets {
		fmt.Fprintf(bw, "// %s is the layout for %s with firmware compat %s.\nvar %s = Layouts{\n", t.Name(), t.G, strings.ReplaceAll(strings.TrimPrefix(t.V, "V"), "_", "."), t.Name())
		lays := all[t]
		var ks []string
		for k := range lays {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			lay := lays[k]
			fmt.Fprintf(bw, "\t%q: {Size: %#x, Align: %d, Fields: []Field{", k, lay.Size, lay.Align)
			for _, f := range lay.Fields {
				fmt.Fprintf(bw, "{%q, %#x, %#x, %q}, ", f.Name, f.Off, f.Size, f.Type)
			}
			fmt.Fprintf(bw, "}},\n")
		}
		fmt.Fprintf(bw, "}\n\n")
	}
	code, err := format.Source(bw.Bytes())
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "agxlayout:", err)
	os.Exit(1)
}
