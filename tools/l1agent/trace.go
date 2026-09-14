package main

// The ftrace and dmesg reports, as functions from the text to the lines the
// bash printed (before their "L1: ..." prefix).  Each reads its input once,
// line by line: the trace buffer is sized in hundreds of megabytes.

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// hcNames: "HYPERCALL" on its own says almost nothing -- a TLB flush and a
// page-table load cost very different amounts -- so pvm_get_exit_info()
// puts which one it was in info2.  Substitute it, which is what that field
// is there for.
var hcNames = map[string]string{
	"20001": "HC_IRQ_WIN", "20002": "HC_IRQ_HALT",
	"20003": "HC_LOAD_PGTBL", "20004": "HC_TLB_FLUSH",
	"20005": "HC_TLB_FLUSH_CURRENT",
	"20006": "HC_TLB_INVLPG",
	"20007": "HC_LOAD_GS", "20008": "HC_RDMSR",
	"20009": "HC_WRMSR", "2000a": "HC_LOAD_TLS",
}

// hcReason is the awk that renames a HYPERCALL exit by its info2.
func hcReason(r, info2 string) string {
	if r != "HYPERCALL" {
		return r
	}
	v := info2
	if strings.HasPrefix(v, "0x") { // sub(/^0x0*/, "", v)
		v = strings.TrimLeft(v[2:], "0")
	}
	if n, ok := hcNames[v]; ok {
		return n
	}
	return "HYPERCALL(" + v + ")"
}

// The sed expressions, translated.  Each starts with a greedy ".*", so the
// match starts at the beginning of the line and prefers the last place the
// rest fits, as in sed; the trailing ".*" is left out, since it changes
// neither whether a line matches nor what is captured.  The literal each
// needs is checked first, which is only a speed-up.
var (
	exitRe    = regexp.MustCompile(`.*kvm_exit: .* reason (.*) rip .*info2 (0x[0-9a-f]*)`)
	exitRipRe = regexp.MustCompile(`.*kvm_exit: .* reason (.*) rip (0x[0-9a-f]*).*info2 (0x[0-9a-f]*)`)
	insnRe    = regexp.MustCompile(`.*kvm_emulate_insn: [^:]*:[^:]*:([0-9a-f ]*)\(`)
	msrRe     = regexp.MustCompile(`.*kvm_msr: msr_([a-z]*) ([0-9a-f]*) `)
)

var (
	litExit = []byte("kvm_exit: ")
	litInsn = []byte("kvm_emulate_insn: ")
	litMsr  = []byte("kvm_msr: msr_")
)

// awkSplitBar is $1, $2, $3 of `awk -F'|'`.
func awkSplitBar(s string) []string { return strings.Split(s, "|") }

// ExitsByReason is the "exits by reason" histogram, top 18.
func ExitsByReason(r io.Reader) []string {
	n := map[string]int{}
	eachLine(r, func(l []byte, _ bool) bool {
		if !bytes.Contains(l, litExit) {
			return true
		}
		if m := exitRe.FindSubmatch(l); m != nil {
			// sed prints "\1|\2", awk splits it again on '|'.
			f := awkSplitBar(string(m[1]) + "|" + string(m[2]))
			n[hcReason(field(f, 1), field(f, 2))]++
		}
		return true
	})
	var out []string
	for k, v := range n {
		out = append(out, fmt.Sprintf("%8d  %s", v, k))
	}
	sortRN(out)
	return head(out, 18)
}

// ExitsByRip is the "exits by reason and guest rip" histogram, top 25.
func ExitsByRip(r io.Reader) []string {
	type key struct{ reason, rip string }
	n := map[key]int{}
	eachLine(r, func(l []byte, _ bool) bool {
		if !bytes.Contains(l, litExit) {
			return true
		}
		if m := exitRipRe.FindSubmatch(l); m != nil {
			f := awkSplitBar(string(m[1]) + "|" + string(m[2]) + "|" + string(m[3]))
			n[key{hcReason(field(f, 1), field(f, 3)), field(f, 2)}]++
		}
		return true
	})
	var out []string
	for k, v := range n {
		out = append(out, fmt.Sprintf("%8d  %s %s", v, padRight(k.reason, 22), k.rip))
	}
	sortRN(out)
	return head(out, 25)
}

// insnBytes is the opcode bytes sed pulls out of a kvm_emulate_insn line.
func insnBytes(l []byte) (string, bool) {
	if !bytes.Contains(l, litInsn) {
		return "", false
	}
	m := insnRe.FindSubmatch(l)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// CountLines is grep -c <lit>: lines containing it.
func CountLines(r io.Reader, lit string) int {
	b := []byte(lit)
	c := 0
	eachLine(r, func(l []byte, _ bool) bool {
		if bytes.Contains(l, b) {
			c++
		}
		return true
	})
	return c
}

// InsnHistogram is the emulated instructions by their first three bytes,
// top 20.
func InsnHistogram(r io.Reader) []string {
	n := map[string]int{}
	eachLine(r, func(l []byte, _ bool) bool {
		if s, ok := insnBytes(l); ok {
			f := awkFields(s)
			n[field(f, 1)+" "+field(f, 2)+" "+field(f, 3)]++
		}
		return true
	})
	out := uniqCount(n)
	sortRN(out)
	return head(out, 20)
}

// privName is the awk that names a privileged instruction.
func privName(f []string) string {
	// Skip operand-size, address-size and REX prefixes.
	i := 1
	for {
		p := field(f, i)
		if p == "66" || p == "67" || (len(p) == 2 && p[0] == '4' && isHexLower(p[1])) ||
			p == "f2" || p == "f3" || p == "2e" || p == "3e" ||
			p == "26" || p == "36" || p == "64" || p == "65" {
			i++
			continue
		}
		break
	}
	op, op2 := field(f, i), field(f, i+1)
	if op == "0f" {
		switch op2 {
		case "20":
			return "mov %crN,%reg"
		case "22":
			return "mov %reg,%crN"
		case "21":
			return "mov %drN,%reg"
		case "23":
			return "mov %reg,%drN"
		case "30":
			return "wrmsr"
		case "32":
			return "rdmsr"
		case "31":
			return "rdtsc"
		case "a2":
			return "cpuid"
		case "01":
			return "lgdt/lidt/etc"
		case "06":
			return "clts"
		case "09":
			return "wbinvd"
		case "00":
			return "lldt/ltr/etc"
		case "07":
			return "sysret"
		case "05":
			return "syscall"
		}
		return ""
	}
	switch op {
	case "ec", "ed":
		return "in dx"
	case "ee", "ef":
		return "out dx"
	case "e4", "e5":
		return "in imm"
	case "e6", "e7":
		return "out imm"
	case "6c", "6d":
		return "ins"
	case "6e", "6f":
		return "outs"
	case "fa":
		return "cli"
	case "fb":
		return "sti"
	case "f4":
		return "hlt"
	}
	return ""
}

func isHexLower(c byte) bool { return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') }

// PrivHistogram is the privileged instructions only, all of them.
func PrivHistogram(r io.Reader) []string {
	n := map[string]int{}
	eachLine(r, func(l []byte, _ bool) bool {
		if s, ok := insnBytes(l); ok {
			if name := privName(awkFields(s)); name != "" {
				n[name]++
			}
		}
		return true
	})
	var out []string
	for k, v := range n {
		out = append(out, fmt.Sprintf("%8d  %s", v, k))
	}
	sortRN(out)
	return out
}

// MSRHistogram is the MSR accesses by register, top 12.
func MSRHistogram(r io.Reader) []string {
	n := map[string]int{}
	eachLine(r, func(l []byte, _ bool) bool {
		if !bytes.Contains(l, litMsr) {
			return true
		}
		if m := msrRe.FindSubmatch(l); m != nil {
			n[string(m[1])+" "+string(m[2])]++
		}
		return true
	})
	out := uniqCount(n)
	sortRN(out)
	return head(out, 12)
}

// traceNoise is what the last-lines view drops: the teardown thread and
// the mmu-notifier unmap storm.  The bash's
// "kvm_(pic|ioapic)_set_irq" is spelled out.
var traceNoise = [][]byte{
	[]byte("kvm-nx-lpage"), []byte("kvm_unmap_hva_range"), []byte("kvm_hv_stimer_cleanup"),
	[]byte("kvm-pit"), []byte("kvm_pic_set_irq"), []byte("kvm_ioapic_set_irq"), []byte("kvm_set_irq"),
}

// LastTraceLines is grep -vE <noise> | tail -n.
func LastTraceLines(r io.Reader, n int) []string {
	ring := make([]string, 0, n)
	pos := 0
	eachLine(r, func(l []byte, _ bool) bool {
		for _, x := range traceNoise {
			if bytes.Contains(l, x) {
				return true
			}
		}
		if len(ring) < n {
			ring = append(ring, string(l))
		} else {
			ring[pos] = string(l)
			pos = (pos + 1) % n
		}
		return true
	})
	return append(ring[pos:len(ring):len(ring)], ring[:pos]...)
}

// MMUCounts is grep -E "kvmmmu:|kvm:|seconds" on perf stat's output.
func MMUCounts(b []byte) []string {
	return grepLines(b, func(l string) bool {
		return strings.Contains(l, "kvmmmu:") || strings.Contains(l, "kvm:") || strings.Contains(l, "seconds")
	})
}

// --- dmesg -----------------------------------------------------------------

// DmesgKVM is dmesg | grep -i -E 'pvm|kvm' | tail -n.
func DmesgKVM(b []byte, n int) []string {
	return tail(grepLines(b, func(l string) bool {
		return containsFold(l, "pvm") || containsFold(l, "kvm")
	}), n)
}

// DmesgPVM is dmesg | grep -iE "PVM:|non-PVM mode" | tail -6.
func DmesgPVM(b []byte) []string {
	return tail(grepLines(b, func(l string) bool {
		return containsFold(l, "pvm:") || containsFold(l, "non-pvm mode")
	}), 6)
}

// DmesgWarn is dmesg | grep -i "non-PVM mode" | tail -3 | grep .
func DmesgWarn(b []byte) []string {
	return dropEmpty(tail(grepLines(b, func(l string) bool { return containsFold(l, "non-pvm mode") }), 3))
}

var sigRe = regexp.MustCompile(`segfault|trap [a-z]+ ip|traps:`)

// DmesgSig is dmesg | grep -E "segfault|trap [a-z]+ ip|traps:" | tail -5 | grep .
func DmesgSig(b []byte) []string {
	return dropEmpty(tail(grepLines(b, func(l string) bool { return sigRe.MatchString(l) }), 5))
}

// DmesgBug is
//
//	dmesg | sed -n '/scheduling while atomic\|BUG:\|general protection fault\|unable to handle/,+12p' | head -30 | grep .
func DmesgBug(b []byte) []string {
	r := sedRangePlus(splitLines(b), func(l string) bool {
		return strings.Contains(l, "scheduling while atomic") || strings.Contains(l, "BUG:") ||
			strings.Contains(l, "general protection fault") || strings.Contains(l, "unable to handle")
	}, 12)
	return dropEmpty(head(r, 30))
}

// dropEmpty is grep . -- drop empty lines.
func dropEmpty(lines []string) []string {
	var out []string
	for _, l := range lines {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
