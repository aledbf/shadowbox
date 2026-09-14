package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	l1Head = "root=/dev/vda rw console=ttyS0,115200 panic=-1"
	l1Tail = "systemd.mask=serial-getty@ttyS0.service systemd.show_status=false"
)

// cmdlineCases are the L1 command lines the goldens were recorded for from the bash agent, whose
// expectations the bash agent's own code produced.  Keep the two in step.
var cmdlineCases = []struct{ name, cmdline string }{
	{"default", l1Head + " pvmtest.suite=default pvmtest.vendor=pvm      " + l1Tail},
	{"bare", l1Head + " " + l1Tail},
	{"perf-intel", l1Head + " pvmtest.suite=perf pvmtest.vendor=intel   pvmtest.guest_cpus=4 pvmtest.guest_mem=2G  " + l1Tail},
	{"profile", l1Head + " pvmtest.suite=profile pvmtest.vendor=pvm pvmtest.profile_case=perf/context-switch   " + l1Tail},
	{"lock", l1Head + " pvmtest.suite=lock pvmtest.vendor=pvm     " + l1Tail},
	{"mmu-intel", l1Head + " pvmtest.suite=mmu pvmtest.vendor=intel  pvmtest.guest_append=pvmtest.only=perf/syscall,pvmtest.statsmsr=1   " + l1Tail},
	{"hosttests", l1Head + " pvmtest.suite=hosttests pvmtest.vendor=pvm  pvmtest.mod_args=foo=1,bar=2   " + l1Tail + " pvmtest.test_timeout=900"},
	{"failclosed", l1Head + " pvmtest.suite=failclosed pvmtest.vendor=pvm     pti=on " + l1Tail},
	{"full-tcg", l1Head + " pvmtest.suite=full pvmtest.vendor=intel     pvmtest.guest_timeout=15000 " + l1Tail},
	{"all", l1Head + " pvmtest.suite=all pvmtest.vendor=pvm " + l1Tail},
	{"unknown", l1Head + " pvmtest.suite=smoke pvmtest.vendor=amd " + l1Tail},
	{"empties", l1Head + " pvmtest.suite= pvmtest.vendor= pvmtest.guest_cpus= pvmtest.guest_mem= pvmtest.mod_args= pvmtest.test_timeout=abc pvmtest.guest_timeout=x1 " + l1Tail},
	{"lastwins", l1Head + " pvmtest.suite=smoke pvmtest.guest_cpus=1 pvmtest.vendor=intel pvmtest.guest_cpus=8 xpvmtest.guest_mem=3G pvmtest.test_timeout=12abc " + l1Tail},
	{"nested", l1Head + " pvmtest.suite=security pvmtest.guest_append=pvmtest.suite=perf,pvmtest.only=x " + l1Tail},
	{"echo-n", l1Head + " pvmtest.suite=smoke pvmtest.guest_append=-n " + l1Tail},
	{"echo-neE", l1Head + " pvmtest.suite=perf pvmtest.guest_append=-neE pvmtest.vendor=intel " + l1Tail},
	{"echo-dash", l1Head + " pvmtest.suite=smoke pvmtest.guest_append=-,-x,-n,x " + l1Tail},
	{"tabs", l1Head + " pvmtest.mod_args=a\tb,c,,d pvmtest.guest_mem=1G\tx " + l1Tail},
}

func brackets(argv []string) string {
	var b strings.Builder
	for _, a := range argv {
		b.WriteString("[" + a + "]")
	}
	return b.String()
}

// renderCmdline prints what the recording printed for one command line, from the
// Go functions.
func renderCmdline(cmdline string) string {
	var b strings.Builder
	// /proc/cmdline ends in a newline; the recording's printf '%s\n' too.
	p := ParseCmdline(cmdline + "\n")
	b.WriteString("vendor=" + p.Vendor + "\n")
	mod, ok := ModulePath(p.Vendor)
	if !ok {
		b.WriteString("unknown\n")
		return b.String()
	}
	b.WriteString("L1: loading " + p.Vendor + " from " + mod + " " + p.ModArgs + "\n")
	b.WriteString("insmod=" + brackets(InsmodArgv(mod, p.ModArgs)) + "\n")
	mode, suite := ResolveMode(p.Suite)
	b.WriteString("mode=" + mode + " suite=" + suite + "\n")
	b.WriteString("test_timeout=" + p.TestTimeout + "\n")
	gt := GuestTimeout(suite, p.GuestTimeout)
	b.WriteString("L1: guest: " + p.GuestCPUs + " vcpus, " + p.GuestMem + ", timeout " + gt + "s\n")
	app := GuestAppend(p, mode, suite)
	b.WriteString("append=" + app + "\n")
	prefix := PerfPrefix(PerfKind(mode, suite, true))
	b.WriteString("qemu=" + brackets(QemuArgv(gt, prefix, []string{"-machine", "q35,accel=kvm"}, p.GuestCPUs, p.GuestMem, app)) + "\n")
	return b.String()
}

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCmdlineGolden(t *testing.T) {
	for _, c := range cmdlineCases {
		t.Run(c.name, func(t *testing.T) {
			want := golden(t, "cmdline-"+c.name+".golden")
			if got := renderCmdline(c.cmdline); got != want {
				t.Errorf("got:\n%s\nwant (bash):\n%s", got, want)
			}
		})
	}
}

func TestParseCmdlineDefaults(t *testing.T) {
	got := ParseCmdline("")
	want := Params{Vendor: "pvm", Suite: "default", GuestCPUs: "2", GuestMem: "1G", TestTimeout: "300"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveMode(t *testing.T) {
	for suite, want := range map[string][2]string{
		"default":    {modeRun, "default"},
		"smoke":      {modeRun, "smoke"},
		"perf":       {modeRun, "perf"},
		"failclosed": {modeRun, "failclosed"},
		"profile":    {modeProfile, "perf"},
		"lock":       {modeLock, "perf"},
		"mmu":        {modeMMU, "perf"},
		"hosttests":  {modeHosttests, "hosttests"},
	} {
		m, s := ResolveMode(suite)
		if m != want[0] || s != want[1] {
			t.Errorf("%s: got %s/%s, want %s/%s", suite, m, s, want[0], want[1])
		}
	}
}

func TestTraceSetup(t *testing.T) {
	base := []traceWrite{{"tracing_on", "0\n", ""}, {"trace", "\n", ""}}
	on := traceWrite{"tracing_on", "1\n", ""}
	perf := []traceWrite{
		{"buffer_size_kb", "131072\n", ""},
		{"events/kvm/kvm_emulate_insn/enable", "1\n", "tracing emulated instructions"},
		{"events/kvm/kvm_msr/enable", "1\n", ""},
	}
	cat := func(parts ...[]traceWrite) []traceWrite {
		var out []traceWrite
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	cases := []struct {
		mode, suite string
		want        []traceWrite
	}{
		{modeProfile, "perf", cat(base, []traceWrite{on})},
		{modeRun, "perf", cat(base, perf, []traceWrite{on})},
		{modeLock, "perf", cat(base, perf, []traceWrite{on})},
		{modeMMU, "perf", cat(base, perf, []traceWrite{{"events/kvm/kvm_exit/enable", "1\n", "tracing exit reasons"}, on})},
		{modeRun, "default", cat(base, []traceWrite{{"buffer_size_kb", "32768\n", ""}, {"events/kvm/enable", "1\n", "kvm tracepoints enabled"}, on})},
		{modeHosttests, "hosttests", cat(base, []traceWrite{{"buffer_size_kb", "32768\n", ""}, {"events/kvm/enable", "1\n", "kvm tracepoints enabled"}, on})},
	}
	for _, c := range cases {
		if got := TraceSetup(c.mode, c.suite); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s/%s:\n got %q\nwant %q", c.mode, c.suite, got, c.want)
		}
	}
}

func TestPerfKind(t *testing.T) {
	cases := []struct {
		mode, suite string
		ok          bool
		want        perfKind
	}{
		{modeMMU, "perf", true, perfMMU},
		{modeLock, "perf", true, perfLock},
		{modeProfile, "perf", true, perfProfile},
		{modeRun, "perf", true, perfKVMStat},
		{modeRun, "default", true, perfNone},
		{modeHosttests, "hosttests", true, perfNone},
		{modeMMU, "perf", false, perfNone},
		{modeRun, "perf", false, perfNone},
	}
	for _, c := range cases {
		if got := PerfKind(c.mode, c.suite, c.ok); got != c.want {
			t.Errorf("%s/%s/%v: got %d, want %d", c.mode, c.suite, c.ok, got, c.want)
		}
	}
	if got := KVMStatProbeArgv(); brackets(got) != "[/mnt/payload/perf][kvm][stat][record][-a][--][true]" {
		t.Errorf("probe: %v", got)
	}
}

func TestReportArgv(t *testing.T) {
	cases := []struct {
		got  []string
		want string
	}{
		{profileReportArgv("--percent-limit", "0.4", "-g", "none"),
			"/mnt/payload/perf report -i /tmp/cycles.data --stdio --sort symbol --percent-limit 0.4 -g none"},
		{profileReportArgv("--no-children", "--percent-limit", "0.3", "-g", "none"),
			"/mnt/payload/perf report -i /tmp/cycles.data --stdio --sort symbol --no-children --percent-limit 0.3 -g none"},
		{lockContentionArgv("-E", "12", "-l", "-F", "contended,wait_total,avg_wait"),
			"/mnt/payload/perf lock contention -i /tmp/lock.data -q -k wait_total -E 12 -l -F contended,wait_total,avg_wait"},
		{lockContentionArgv("-E", "8", "-S", "kvm_mmu_page_fault", "-F", "contended,wait_total,wait_max,avg_wait"),
			"/mnt/payload/perf lock contention -i /tmp/lock.data -q -k wait_total -E 8 -S kvm_mmu_page_fault -F contended,wait_total,wait_max,avg_wait"},
		{kvmStatReportArgv("/tmp/perf.data.kvm"), "/mnt/payload/perf kvm -i /tmp/perf.data.kvm stat report --stdio"},
		{TestArgv("300", "/mnt/payload/hosttests/pvm_abi_test"), "timeout -k 5 300 /mnt/payload/hosttests/pvm_abi_test"},
	}
	for _, c := range cases {
		if s := strings.Join(c.got, " "); s != c.want {
			t.Errorf("got  %s\nwant %s", s, c.want)
		}
	}
}

func TestSelftests(t *testing.T) {
	for in, want := range map[string]string{
		"dirty_log_test":           "dirty_log_test",
		"x86__cpuid_test":          "x86/cpuid_test",
		"x86__a__b":                "x86/a__b",
		"no_separator_here_at_all": "no_separator_here_at_all",
	} {
		if got := SelftestName(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
	for rc, want := range map[int]string{0: "pass", 4: "skip", 124: "timeout", 137: "timeout", 1: "fail", 126: "fail", 127: "fail", 139: "fail"} {
		if got := SelftestVerdict(rc); got != want {
			t.Errorf("rc=%d: got %s, want %s", rc, got, want)
		}
	}
}

func TestKUT(t *testing.T) {
	got := SplitQuoted(`-cpu max -append 'a b' "c"`)
	want := []string{"-cpu", "max", "-append", "a b", "c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", got)
	}
	m := ParseKUTManifest([]byte("# x\nx2apic\tvmexit.flat\t2\t90\t\t-append 'toggle_cr4_pge'\n"))
	if len(m) != 1 || m[0].Smp != "2" || strings.Join(m[0].Args, "|") != "-append|toggle_cr4_pge" {
		t.Fatalf("%+v", m)
	}
	for rc, v := range map[int]string{1: "pass", 3: "fail", 77: "skip", 124: "timeout", 0: "error"} {
		if KUTVerdict(rc) != v {
			t.Fatalf("rc %d: %s", rc, KUTVerdict(rc))
		}
	}
	read := func(p string) (string, error) { return map[string]string{"/e": "N\n"}[p], nil }
	if c := KUTCheck("/e=N", read); c != "" {
		t.Fatal(c)
	}
	if c := KUTCheck("/e=Y", read); c != "/e=Y" {
		t.Fatal(c)
	}
}
