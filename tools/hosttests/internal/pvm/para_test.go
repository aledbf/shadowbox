package pvm

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// The PVCS layout as the header had it when this was written.  Without KSRC
// this is the only check; with it, TestHeaderMatches is the real one.
func TestPVCSLayout(t *testing.T) {
	cases := []struct {
		name      string
		got, want uintptr
	}{
		{"event_flags", PVCS_EVENT_FLAGS, 0},
		{"cr2", PVCS_CR2, 8},
		{"user_cs", PVCS_USER_CS, 64},
		{"user_ss", PVCS_USER_SS, 66},
		{"event_errcode", PVCS_EVENT_ERRCODE, 68},
		{"event_vector", PVCS_EVENT_VECTOR, 70},
		{"user_gsbase", PVCS_USER_GSBASE, 72},
		{"eflags", PVCS_EFLAGS, 80},
		{"pkru", PVCS_PKRU, 84},
		{"rip", PVCS_RIP, 88},
		{"rcx", PVCS_RCX, 96},
		{"r11", PVCS_R11, 104},
		{"sizeof", PVCS_SIZE, 128},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("pvm_vcpu_struct.%s at %d, want %d", c.name, c.got, c.want)
		}
	}
}

// goConstants is every numeric constant in para.go, under its header name.
var goConstants = map[string]uint64{
	"PVM_SYNTHETIC_CPUID_ADDRESS":       PVM_SYNTHETIC_CPUID_ADDRESS,
	"PVM_CPUID_SIGNATURE":               PVM_CPUID_SIGNATURE,
	"PVM_CPUID_FEATURES":                PVM_CPUID_FEATURES,
	"PVM_CPUID_MAX":                     PVM_CPUID_MAX,
	"PVM_ABI_VERSION":                   PVM_ABI_VERSION,
	"PVM_FEATURE_DIRECT_PF_BIT":         PVM_FEATURE_DIRECT_PF_BIT,
	"PVM_FEATURE_DIRECT_PF":             PVM_FEATURE_DIRECT_PF,
	"PVM_VIRTUAL_MSR_BASE":              PVM_VIRTUAL_MSR_BASE,
	"MSR_PVM_VCPU_STRUCT":               MSR_PVM_VCPU_STRUCT,
	"MSR_PVM_EVENT_ENTRY":               MSR_PVM_EVENT_ENTRY,
	"MSR_PVM_RETU_RIP":                  MSR_PVM_RETU_RIP,
	"MSR_PVM_FEATURES_ENABLED":          MSR_PVM_FEATURES_ENABLED,
	"PVM_HC_SPECIAL_BASE":               PVM_HC_SPECIAL_BASE,
	"PVM_HC_LOAD_PGTBL":                 PVM_HC_LOAD_PGTBL,
	"PVM_HC_IRQ_WIN":                    PVM_HC_IRQ_WIN,
	"PVM_HC_IRQ_HLT":                    PVM_HC_IRQ_HLT,
	"PVM_EVENT_ENTRY_SUPERVISOR_OFFSET": PVM_EVENT_ENTRY_SUPERVISOR_OFFSET,
	"PVM_HC_TLB_FLUSH":                  PVM_HC_TLB_FLUSH,
	"PVM_HC_TLB_FLUSH_CURRENT":          PVM_HC_TLB_FLUSH_CURRENT,
	"PVM_HC_TLB_INVLPG":                 PVM_HC_TLB_INVLPG,
	"PVM_HC_LOAD_GS":                    PVM_HC_LOAD_GS,
	"PVM_HC_RDMSR":                      PVM_HC_RDMSR,
	"PVM_HC_WRMSR":                      PVM_HC_WRMSR,
	"PVM_HC_LOAD_TLS":                   PVM_HC_LOAD_TLS,
	"PVM_EVENT_FLAGS_IP_BIT":            PVM_EVENT_FLAGS_IP_BIT,
	"PVM_EVENT_FLAGS_IP":                PVM_EVENT_FLAGS_IP,
	"PVM_EVENT_FLAGS_IF_BIT":            PVM_EVENT_FLAGS_IF_BIT,
	"PVM_EVENT_FLAGS_IF":                PVM_EVENT_FLAGS_IF,
	"PVM_PVCS_EVENT_VECTOR_STD_BIT":     PVM_PVCS_EVENT_VECTOR_STD_BIT,
	"PVM_PVCS_EVENT_VECTOR_STD":         PVM_PVCS_EVENT_VECTOR_STD,
	"PVM_PVCS_EVENT_VECTOR_NMI_BIT":     PVM_PVCS_EVENT_VECTOR_NMI_BIT,
	"PVM_PVCS_EVENT_VECTOR_NMI":         PVM_PVCS_EVENT_VECTOR_NMI,
	"PVM_PVCS_EVENT_VECTOR_MCE_BIT":     PVM_PVCS_EVENT_VECTOR_MCE_BIT,
	"PVM_PVCS_EVENT_VECTOR_MCE":         PVM_PVCS_EVENT_VECTOR_MCE,
	"PVM_LOAD_PGTBL_FLAGS_TLB":          PVM_LOAD_PGTBL_FLAGS_TLB,
	"PVM_LOAD_PGTBL_FLAGS_LA57":         PVM_LOAD_PGTBL_FLAGS_LA57,
}

// TestHeaderMatches holds this package to $KSRC's pvm_para.h: every
// #define and every field of struct pvm_vcpu_struct, in both directions, so
// that a constant changed, added or removed on the kernel side is noticed.
func TestHeaderMatches(t *testing.T) {
	ksrc := os.Getenv("KSRC")
	if ksrc == "" {
		t.Skip("KSRC is not set")
	}
	path := filepath.Join(ksrc, "arch/x86/include/uapi/asm/pvm_para.h")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := stripComments(string(src))

	defs := parseDefines(text)
	if len(defs) == 0 {
		t.Fatalf("%s: no #defines found", path)
	}

	seen := map[string]bool{}
	for name, body := range defs {
		switch name {
		case "_UAPI_ASM_X86_PVM_PARA_H":
			continue
		case "PVM_SYNTHETIC_CPUID":
			got, err := parseByteList(body)
			if err != nil {
				t.Errorf("PVM_SYNTHETIC_CPUID: %v", err)
			} else if !reflect.DeepEqual(got, PVM_SYNTHETIC_CPUID[:]) {
				t.Errorf("PVM_SYNTHETIC_CPUID is % x in the header, % x here", got, PVM_SYNTHETIC_CPUID[:])
			}
			continue
		case "PVM_SIGNATURE":
			got, err := strconv.Unquote(strings.ReplaceAll(body, `\0`, `\x00`))
			if err != nil {
				t.Errorf("PVM_SIGNATURE %s: %v", body, err)
			} else if got != PVM_SIGNATURE {
				t.Errorf("PVM_SIGNATURE is %q in the header, %q here", got, PVM_SIGNATURE)
			}
			continue
		}
		seen[name] = true
		want, ok := goConstants[name]
		if !ok {
			t.Errorf("%s is defined in the header but not in package pvm", name)
			continue
		}
		got, err := evalDefine(name, defs, 0)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%s is %#x in the header, %#x here", name, got, want)
		}
	}
	for name := range goConstants {
		if !seen[name] {
			t.Errorf("%s is in package pvm but no longer in the header", name)
		}
	}

	fields, size, err := parseStruct(text, "pvm_vcpu_struct")
	if err != nil {
		t.Fatal(err)
	}
	typ := reflect.TypeOf(VcpuStruct{})
	if typ.NumField() != len(fields) {
		t.Errorf("struct pvm_vcpu_struct has %d fields, VcpuStruct %d", len(fields), typ.NumField())
	}
	for i := 0; i < typ.NumField() && i < len(fields); i++ {
		f, c := typ.Field(i), fields[i]
		if f.Tag.Get("c") != c.name || f.Offset != c.offset || f.Type.Size() != c.size {
			t.Errorf("field %d: header has %s at %d size %d, VcpuStruct has %s at %d size %d",
				i, c.name, c.offset, c.size, f.Tag.Get("c"), f.Offset, f.Type.Size())
		}
	}
	if size != unsafe.Sizeof(VcpuStruct{}) {
		t.Errorf("sizeof(struct pvm_vcpu_struct) is %d, VcpuStruct %d", size, unsafe.Sizeof(VcpuStruct{}))
	}
}

func stripComments(s string) string {
	s = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(s, " ")
	return regexp.MustCompile(`//[^\n]*`).ReplaceAllString(s, "")
}

// parseDefines returns every object-like #define and its body, with line
// continuations joined.
func parseDefines(text string) map[string]string {
	text = strings.ReplaceAll(text, "\\\n", " ")
	re := regexp.MustCompile(`(?m)^\s*#\s*define\s+([A-Za-z_][A-Za-z0-9_]*)([ \t]+([^\n]*))?$`)
	defs := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		defs[m[1]] = strings.TrimSpace(m[3])
	}
	return defs
}

func parseByteList(body string) ([]byte, error) {
	var out []byte
	for _, f := range strings.Split(body, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(f), 0, 8)
		if err != nil {
			return nil, err
		}
		out = append(out, byte(v))
	}
	return out, nil
}

var tokenRE = regexp.MustCompile(`\s*(0[xX][0-9a-fA-F]+|[0-9]+|[A-Za-z_][A-Za-z0-9_]*|[()+\-]|<<)`)

// evalDefine evaluates the small expression language the header uses:
// integers, other macros, parentheses, + - << and _BITUL().
func evalDefine(name string, defs map[string]string, depth int) (uint64, error) {
	if depth > 16 {
		return 0, fmt.Errorf("%s: macro recursion", name)
	}
	body, ok := defs[name]
	if !ok {
		return 0, fmt.Errorf("%s is not defined", name)
	}
	var toks []string
	rest := body
	for strings.TrimSpace(rest) != "" {
		m := tokenRE.FindStringSubmatchIndex(rest)
		if m == nil || m[0] != 0 {
			return 0, fmt.Errorf("%s: cannot parse %q", name, body)
		}
		toks = append(toks, rest[m[2]:m[3]])
		rest = rest[m[1]:]
	}
	p := &exprParser{toks: toks, defs: defs, depth: depth, name: name}
	v, err := p.expr()
	if err != nil {
		return 0, err
	}
	if p.pos != len(p.toks) {
		return 0, fmt.Errorf("%s: trailing tokens in %q", name, body)
	}
	return v, nil
}

type exprParser struct {
	toks  []string
	pos   int
	defs  map[string]string
	depth int
	name  string
}

func (p *exprParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *exprParser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *exprParser) expr() (uint64, error) {
	v, err := p.sum()
	for err == nil && p.peek() == "<<" {
		p.next()
		var r uint64
		r, err = p.sum()
		v <<= r
	}
	return v, err
}

func (p *exprParser) sum() (uint64, error) {
	v, err := p.primary()
	for err == nil && (p.peek() == "+" || p.peek() == "-") {
		op := p.next()
		var r uint64
		r, err = p.primary()
		if op == "+" {
			v += r
		} else {
			v -= r
		}
	}
	return v, err
}

func (p *exprParser) primary() (uint64, error) {
	t := p.next()
	switch {
	case t == "(":
		v, err := p.expr()
		if err == nil && p.next() != ")" {
			err = fmt.Errorf("%s: missing )", p.name)
		}
		return v, err
	case t == "_BITUL" || t == "_BITULL":
		if p.next() != "(" {
			return 0, fmt.Errorf("%s: %s without (", p.name, t)
		}
		v, err := p.expr()
		if err == nil && p.next() != ")" {
			err = fmt.Errorf("%s: missing )", p.name)
		}
		return 1 << v, err
	case t != "" && t[0] >= '0' && t[0] <= '9':
		return strconv.ParseUint(t, 0, 64)
	case t != "" && (t[0] == '_' || t[0] >= 'A'):
		return evalDefine(t, p.defs, p.depth+1)
	}
	return 0, fmt.Errorf("%s: unexpected %q", p.name, t)
}

type cField struct {
	name         string
	offset, size uintptr
}

// parseStruct lays out a struct made of __u8/__u16/__u32/__u64 scalars and
// arrays with the x86_64 ABI's alignment rules.
func parseStruct(text, tag string) ([]cField, uintptr, error) {
	re := regexp.MustCompile(`(?s)struct\s+` + tag + `\s*\{(.*?)\};`)
	m := re.FindStringSubmatch(text)
	if m == nil {
		return nil, 0, fmt.Errorf("struct %s not found", tag)
	}
	sizes := map[string]uintptr{"__u8": 1, "__u16": 2, "__u32": 4, "__u64": 8}
	declRE := regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?:\[(\d+)\])?$`)

	var fields []cField
	var off, maxAlign uintptr = 0, 1
	for _, decl := range strings.Split(m[1], ";") {
		decl = strings.TrimSpace(decl)
		if decl == "" {
			continue
		}
		sp := strings.IndexAny(decl, " \t\n")
		if sp < 0 {
			return nil, 0, fmt.Errorf("cannot parse %q", decl)
		}
		typ := decl[:sp]
		esize, ok := sizes[typ]
		if !ok {
			return nil, 0, fmt.Errorf("unsupported type %q in %q", typ, decl)
		}
		if esize > maxAlign {
			maxAlign = esize
		}
		for _, n := range strings.Split(decl[sp:], ",") {
			dm := declRE.FindStringSubmatch(strings.TrimSpace(n))
			if dm == nil {
				return nil, 0, fmt.Errorf("cannot parse declarator %q", n)
			}
			count := uintptr(1)
			if dm[2] != "" {
				c, _ := strconv.Atoi(dm[2])
				count = uintptr(c)
			}
			off = (off + esize - 1) &^ (esize - 1)
			fields = append(fields, cField{dm[1], off, esize * count})
			off += esize * count
		}
	}
	size := (off + maxAlign - 1) &^ (maxAlign - 1)
	return fields, size, nil
}
