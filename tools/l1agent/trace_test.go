package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The expectations in testdata/golden/ are the bash pipelines' own output
// over the inputs in testdata/, recorded from the bash agent (scripts/l1-agent.sh, removed when this port replaced it).

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func diff(t *testing.T, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			t.Fatalf("first difference at line %d:\n got  %q\n want %q", i+1, gl, wl)
		}
	}
	t.Fatalf("outputs differ")
}

func TestTraceGolden(t *testing.T) {
	for dir, g := range map[string]string{"testdata": "trace.golden", "testdata/edge": "trace-edge.golden"} {
		t.Run(g, func(t *testing.T) { traceGolden(t, dir, g) })
	}
}

func traceGolden(t *testing.T, dir, g string) {
	var out bytes.Buffer
	say := func(s string) { out.WriteString("L1: " + s + "\n") }
	trace := func(fn func(r io.Reader)) {
		f, err := os.Open(filepath.Join(dir, "trace"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		fn(f)
	}
	say("--- exits by reason ---")
	trace(func(r io.Reader) { writePrefixed(&out, "L1: exit: ", ExitsByReason(r)) })
	say("--- exits by reason and guest rip ---")
	trace(func(r io.Reader) { writePrefixed(&out, "L1: exitrip: ", ExitsByRip(r)) })
	trace(func(r io.Reader) {
		say("--- emulated instructions: " + strconv.Itoa(CountLines(r, "kvm_emulate_insn")) + " traced ---")
	})
	trace(func(r io.Reader) { writePrefixed(&out, "L1: insn: ", InsnHistogram(r)) })
	say("--- privileged instructions only ---")
	trace(func(r io.Reader) { writePrefixed(&out, "L1: priv: ", PrivHistogram(r)) })
	say("--- MSR accesses by register ---")
	trace(func(r io.Reader) { writePrefixed(&out, "L1: msr: ", MSRHistogram(r)) })
	say("--- dropped by the trace buffer ---")
	if b, err := os.ReadFile(filepath.Join(dir, "per_cpu/cpu0/stats")); err == nil {
		writePrefixed(&out, "L1: insn: cpu0 ", grepLines(b, func(l string) bool { return strings.Contains(l, "overrun") }))
	}
	say("--- shadow MMU event counts ---")
	writePrefixed(&out, "L1: mmu: ", MMUCounts(readTestdata(t, "mmu.txt")))
	say("--- last 70 trace lines, teardown and unmaps removed ---")
	trace(func(r io.Reader) { writePrefixed(&out, "L1: kvm: ", LastTraceLines(r, 70)) })

	diff(t, out.String(), golden(t, g))
}

func TestEmptyCount(t *testing.T) {
	// The agent's own spelling of grep -c's "0" plus the echo's.
	if CountLines(bytes.NewReader(nil), "kvm_emulate_insn") != 0 {
		t.Fatal("count of nothing")
	}
	if got, want := "L1: --- emulated instructions: 0\n0 traced ---\n", golden(t, "emptycount.golden"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDmesgGolden(t *testing.T) {
	d := readTestdata(t, "dmesg.txt")
	var out bytes.Buffer
	say := func(s string) { out.WriteString("L1: " + s + "\n") }
	writePrefixed(&out, "L1: dmesg: ", DmesgKVM(d, 40))
	writePrefixed(&out, "L1: dmesg: ", DmesgKVM(d, 20))
	prefixStream(&out, "L1: dmesg: ", bytes.NewReader(tailBytes(d, 60)), -1)
	say("--- what the host said about the guest ---")
	writePrefixed(&out, "L1: pvm: ", DmesgPVM(d))
	say("--- injection warnings ---")
	writePrefixed(&out, "L1: warn: ", DmesgWarn(d))
	say("--- did qemu fault, and where ---")
	writePrefixed(&out, "L1: sig: ", DmesgSig(d))
	say("--- kernel complaints from the guest run ---")
	writePrefixed(&out, "L1: bug: ", DmesgBug(d))

	diff(t, out.String(), golden(t, "dmesg.golden"))
}

func TestStreamsGolden(t *testing.T) {
	n := readTestdata(t, "noeol.txt")
	var out bytes.Buffer
	prefixStream(&out, "G[q35]: ", bytes.NewReader(n), -1)
	out.WriteString("L1: next\n")
	prefixStream(&out, "S[x/y]: ", bytes.NewReader(tailBytes(n, 15)), -1)
	out.WriteString("L1: next\n")
	prefixStream(&out, "S[x/y]: ", bytes.NewReader(tailBytes(n, 2)), -1)
	out.WriteString("L1: next\n")
	prefixStream(&out, "L1: perf: ", bytes.NewReader(n), 2)
	out.WriteString("L1: next\n")
	lines := splitLines(readTestdata(t, "sortrn.txt"))
	sortRN(lines)
	for _, l := range lines {
		out.WriteString(l + "\n")
	}
	diff(t, out.String(), golden(t, "streams.golden"))
}

func TestTailBytes(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 3, ""},
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\nb\nc\n", 5, "a\nb\nc\n"},
		{"\n\n\n", 2, "\n\n"},
	}
	for _, c := range cases {
		if got := string(tailBytes([]byte(c.in), c.n)); got != c.want {
			t.Errorf("tail -%d %q: got %q want %q", c.n, c.in, got, c.want)
		}
	}
}

func TestEachLineLong(t *testing.T) {
	long := strings.Repeat("x", 200000)
	var got []string
	eachLine(strings.NewReader("a\n"+long+"\nb"), func(l []byte, eol bool) bool {
		got = append(got, string(l)+map[bool]string{true: "$", false: ""}[eol])
		return true
	})
	if len(got) != 3 || got[0] != "a$" || got[1] != long+"$" || got[2] != "b" {
		t.Errorf("got %d lines", len(got))
	}
}

func TestEchoTr(t *testing.T) {
	for in, want := range map[string]string{
		"a,b":   "a b",
		"-n":    "",
		"-e":    "",
		"-nEe":  "",
		"-":     "-",
		"-x":    "-x",
		"-n,a":  "-n a",
		"--":    "--",
		"-nx":   "-nx",
		",,a,,": "  a  ",
	} {
		if got := echoTr(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
