package main

// Everything the agent decides from L1's kernel command line, as pure
// functions: the parameters, the mode, the module arguments, the guest's
// command line and timeout, the tracing set-up, the perf wrapper and the
// qemu invocation.

import (
	"regexp"
	"strings"
)

const (
	payload  = "/mnt/payload"
	perfPath = payload + "/perf"
)

// Params are the pvmtest.* values, with the bash defaults applied.
type Params struct {
	Vendor       string // pvmtest.vendor, default pvm
	Suite        string // pvmtest.suite, default "default"
	GuestCPUs    string // pvmtest.guest_cpus, default 2
	GuestMem     string // pvmtest.guest_mem, default 1G
	ModArgs      string // pvmtest.mod_args with commas turned to spaces, unsplit
	TestTimeout  string // pvmtest.test_timeout (digits only), default 300
	GuestTimeout string // pvmtest.guest_timeout (digits only), "" when unset
	ProfileCase  string // pvmtest.profile_case, "" when unset
	GuestAppend  string // pvmtest.guest_append as given, commas and all
}

// cmdlineValue is
//
//	$(sed -n 's/.*pvmtest\.<key>=\(<class>\).*/\1/p' /proc/cmdline)
//
// The leading ".*" is greedy, so the last occurrence of the key wins, and
// the key matches anywhere -- inside another parameter's value too.
func cmdlineValue(cmdline, key, class string) string {
	re := regexp.MustCompile(`.*pvmtest\.` + regexp.QuoteMeta(key) + `=(` + class + `)`)
	lines := strings.Split(cmdline, "\n")
	if n := len(lines); lines[n-1] == "" {
		lines = lines[:n-1]
	}
	var out strings.Builder
	for _, l := range lines {
		if m := re.FindStringSubmatch(l); m != nil {
			out.WriteString(m[1])
			out.WriteByte('\n')
		}
	}
	return stripNewlines(out.String())
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ParseCmdline reads the parameters out of /proc/cmdline's contents.
func ParseCmdline(cmdline string) Params {
	const word, digits = `[^ ]*`, `[0-9]*`
	return Params{
		Vendor:       orDefault(cmdlineValue(cmdline, "vendor", word), "pvm"),
		Suite:        orDefault(cmdlineValue(cmdline, "suite", word), "default"),
		GuestCPUs:    orDefault(cmdlineValue(cmdline, "guest_cpus", word), "2"),
		GuestMem:     orDefault(cmdlineValue(cmdline, "guest_mem", word), "1G"),
		ModArgs:      strings.ReplaceAll(cmdlineValue(cmdline, "mod_args", word), ",", " "),
		TestTimeout:  orDefault(cmdlineValue(cmdline, "test_timeout", digits), "300"),
		GuestTimeout: cmdlineValue(cmdline, "guest_timeout", digits),
		ProfileCase:  cmdlineValue(cmdline, "profile_case", word),
		GuestAppend:  cmdlineValue(cmdline, "guest_append", word),
	}
}

// ModulePath is the vendor module on the payload share.
func ModulePath(vendor string) (string, bool) {
	switch vendor {
	case "pvm":
		return payload + "/kvm-pvm.ko", true
	case "intel":
		return payload + "/kvm-intel.ko", true
	}
	return "", false
}

// InsmodArgv is insmod "$mod" ${MOD_ARGS:-}.
func InsmodArgv(mod, modArgs string) []string {
	return append([]string{"insmod", mod}, splitIFS(modArgs)...)
}

// Modes.
const (
	modeRun       = "run"
	modeProfile   = "profile"
	modeLock      = "lock"
	modeMMU       = "mmu"
	modeHosttests = "hosttests"
	modeKUT       = "kut"
)

// ResolveMode maps pvmtest.suite to the agent's mode and the suite the
// guest is told to run.
func ResolveMode(suite string) (mode, guestSuite string) {
	switch suite {
	case "profile":
		// "profile" runs the guest's perf suite but samples the host's
		// cycles rather than counting the guest's exits.  The switcher
		// executes at CPL0 in the vCPU thread, so it is host kernel text
		// and shows up here.
		return modeProfile, "perf"
	case "lock":
		// Lock contention for one case, from the
		// lock:contention_begin/end tracepoints (no BPF in this host
		// kernel).  Only contended acquisitions are recorded, so the case
		// runs close to its normal speed -- unlike a lockdep/LOCK_STAT
		// build, which would change the very scaling being asked about.
		return modeLock, "perf"
	case "mmu":
		// Counts rather than samples: the shadow MMU tracepoints are far
		// too hot to buffer, and what is wanted is how often each path is
		// taken, not where.
		return modeMMU, "perf"
	case "hosttests":
		// No guest at all.  These drive the KVM API from L1 directly,
		// which is the only way to reach the PVM MSRs and the PVCS
		// pinning: the guest-side suite runs at guest CPL3 and cannot
		// touch either.
		return modeHosttests, suite
	case "kut":
		// No guest of ours either: kvm-unit-tests under whichever vendor
		// was loaded, which for them is kvm-intel.
		return modeKUT, suite
	}
	return modeRun, suite
}

// GuestTimeout is the bound on the guest's qemu, in seconds.
func GuestTimeout(suite, override string) string {
	t := "120"
	switch suite {
	case "full", "perf", "all":
		t = "1800"
	}
	// Debugging a guest that hangs: the serial log only reaches the
	// console once qemu has exited, so the wait to see anything at all is
	// the whole timeout.  pvmtest.guest_timeout= shortens it.
	if override != "" {
		t = override
	}
	return t
}

// GuestAppend is the guest kernel's command line.  suite is the resolved
// one.
func GuestAppend(p Params, mode, suite string) string {
	s := "console=ttyS0,115200 panic=-1 oops=panic pvmtest.suite=" + suite + " pvmtest.tag=" + p.Vendor + "-guest"
	if suite == "perf" {
		// A quiet boot for the perf suite.  The serial console is a
		// 16550: every character costs a poll of the line status register
		// and a write to the transmit register, and each of those is a
		// #GP the host emulates.  A verbose boot puts tens of thousands of
		// those into the exit histogram and drowns out what the guest
		// actually does.  The harness still needs the console for its
		// result line, which is forty-odd lines rather than thirty
		// thousand characters.
		s += " quiet loglevel=0"
		// One benchmark, so the profile is of the thing being asked about.
		if mode == modeProfile {
			s += " pvmtest.only=" + orDefault(p.ProfileCase, "perf/syscall")
		}
		if mode == modeLock {
			s += " pvmtest.only=" + orDefault(p.ProfileCase, "perf/fault-scaling")
		}
	} else {
		s += " earlyprintk=serial,ttyS0,115200"
	}
	// Extra guest arguments, for every suite: pvmtest.only,
	// pvmtest.statsmsr, the guest's own kernel parameters.  Comma
	// separated on L1's command line.
	if p.GuestAppend != "" {
		s += " " + echoTr(p.GuestAppend)
	}
	// Only a PVM run must have relocated itself; under kvm-intel the same
	// image is an ordinary guest and belongs at the usual address.
	if p.Vendor == "pvm" {
		s += " pvmtest.expect=pvm"
	}
	return s
}

// traceWrite is one `echo <Data> > $T/<File> 2>/dev/null`, followed by
// `&& say <Say>` when Say is set.
type traceWrite struct {
	File, Data, Say string
}

// TraceSetup is the tracefs set-up before the guest boots.
//
// The guest dies before its first printk, so the only account of what
// happened is the host's.  KVM's tracepoints are the closest thing to a
// debugger here.
func TraceSetup(mode, suite string) []traceWrite {
	w := []traceWrite{{"tracing_on", "0\n", ""}, {"trace", "\n", ""}}
	switch {
	case mode == modeProfile:
		// no tracepoints while sampling; they cost 20% of the profile
	case suite == "perf":
		// Just the emulated instructions, with room for a lot of them:
		// the whole kvm event set would overrun the buffer in seconds
		// and the opcode histogram is what the perf suite is for.
		w = append(w,
			traceWrite{"buffer_size_kb", "131072\n", ""},
			traceWrite{"events/kvm/kvm_emulate_insn/enable", "1\n", "tracing emulated instructions"},
			traceWrite{"events/kvm/kvm_msr/enable", "1\n", ""})
		// The counters say how many exits there were; nothing says what
		// they were, because a PVM_HC_* exit has no counter of its own
		// -- kvm:kvm_hypercall fires only in kvm_emulate_hypercall(),
		// which PVM reaches for the KVM-specific hypercalls alone.
		// kvm_exit carries the reason, so in mmu mode trace that too.
		if mode == modeMMU {
			w = append(w, traceWrite{"events/kvm/kvm_exit/enable", "1\n", "tracing exit reasons"})
		}
	default:
		w = append(w,
			traceWrite{"buffer_size_kb", "32768\n", ""},
			traceWrite{"events/kvm/enable", "1\n", "kvm tracepoints enabled"})
	}
	return append(w, traceWrite{"tracing_on", "1\n", ""})
}

// Which perf, if any, wraps the guest's qemu.
type perfKind int

const (
	perfNone perfKind = iota
	perfMMU
	perfLock
	perfProfile
	// perf kvm stat, if the probe says this perf has it.
	perfKVMStat
)

// PerfKind picks the perf wrapper; perfOK is [ -x /mnt/payload/perf ].
func PerfKind(mode, suite string, perfOK bool) perfKind {
	if !perfOK {
		return perfNone
	}
	switch {
	case mode == modeMMU:
		return perfMMU
	case mode == modeLock:
		return perfLock
	case mode == modeProfile:
		return perfProfile
	case suite == "perf":
		return perfKVMStat
	}
	return perfNone
}

// mmuEvents are what perf stat counts in mmu mode.
//
// kvm_exit first, because for a hypervisor it is the number: every other
// count here is a theory about what those exits were.  The rest split them
// -- emulated instructions, PIO, MMIO, MSR accesses -- and the kvmmmu ones
// say how much of it was the shadow MMU.
//
// kvm:kvm_hypercall alone is not enough to see PVM hypercalls: it fires in
// kvm_emulate_hypercall(), which PVM reaches only for the KVM-specific
// ones.  PVM_HC_* are counted as exits, not as hypercalls.
const mmuEvents = "kvm:kvm_exit,kvm:kvm_entry,kvm:kvm_emulate_insn,kvm:kvm_pio,kvm:kvm_mmio,kvm:kvm_msr,kvm:kvm_hypercall" +
	",kvmmmu:kvm_mmu_get_page,kvmmmu:kvm_mmu_prepare_zap_page" +
	",kvmmmu:kvm_mmu_sync_page,kvmmmu:kvm_mmu_unsync_page,kvmmmu:fast_page_fault"

// PerfPrefix is $PERF_PREFIX, word split.  For perfKVMStat it is the
// prefix once the probe has passed.
func PerfPrefix(k perfKind) []string {
	switch k {
	case perfMMU:
		return []string{perfPath, "stat", "-a", "-o", "/tmp/mmu.txt", "-e", mmuEvents, "--"}
	case perfLock:
		return []string{perfPath, "lock", "record", "-a", "-o", "/tmp/lock.data", "--"}
	case perfProfile:
		// Not system-wide: L1 spends most of its time idle, and "-a"
		// buries the switcher under pv_native_safe_halt.  Following the
		// qemu process still catches the switcher, which runs at CPL0 in
		// the vCPU thread.
		return []string{perfPath, "record", "-e", "cycles:k", "-F", "4000", "-g", "-o", "/tmp/cycles.data", "--"}
	case perfKVMStat:
		return []string{perfPath, "kvm", "stat", "record", "-a", "--"}
	}
	return nil
}

// KVMStatProbeArgv asks perf whether it has "kvm stat" at all.
func KVMStatProbeArgv() []string {
	return []string{perfPath, "kvm", "stat", "record", "-a", "--", "true"}
}

// QemuArgv is the guest's qemu, bounded by timeout and wrapped in perf.
func QemuArgv(guestTimeout string, perfPrefix, machine []string, cpus, mem, appendLine string) []string {
	argv := []string{"timeout", "-k", "5", guestTimeout}
	argv = append(argv, perfPrefix...)
	argv = append(argv, "qemu-system-x86_64")
	argv = append(argv, machine...)
	return append(argv,
		"-cpu", "host", "-smp", cpus, "-m", mem,
		"-kernel", payload+"/guest-vmlinux",
		"-initrd", payload+"/initrd.cpio.gz",
		"-append", appendLine,
		"-display", "none", "-monitor", "none", "-serial", "file:/tmp/guest.log",
		"-no-reboot")
}

// TestArgv is one host test or selftest under its bound.
func TestArgv(testTimeout, path string) []string {
	return []string{"timeout", "-k", "5", testTimeout, path}
}

// SelftestName is $(basename "$t" | sed 's/__/\//'): the staged file name
// with its first "__" turned back into the directory separator.
func SelftestName(base string) string {
	return strings.Replace(base, "__", "/", 1)
}

// SelftestVerdict classifies a selftest's status.  Exit codes are the
// kselftest convention: 0 pass, 4 skip, anything else a failure; 124 and
// 137 are timeout's.
func SelftestVerdict(rc int) string {
	switch rc {
	case 0:
		return "pass"
	case 4:
		return "skip"
	case 124, 137:
		return "timeout"
	}
	return "fail"
}

// Perf report commands for the three modes that read their data back.
// VM="" in the bash: no --vmlinux, because L1 boots with KASLR, so the
// link-time addresses in the image do not match the running kernel and
// perf resolves nothing.
func profileReportArgv(extra ...string) []string {
	argv := []string{perfPath, "report", "-i", "/tmp/cycles.data", "--stdio", "--sort", "symbol"}
	return append(argv, extra...)
}

func lockContentionArgv(extra ...string) []string {
	argv := []string{perfPath, "lock", "contention", "-i", "/tmp/lock.data", "-q", "-k", "wait_total"}
	return append(argv, extra...)
}

func kvmStatReportArgv(data string) []string {
	return []string{perfPath, "kvm", "-i", data, "stat", "report", "--stdio"}
}

// KUTTest is one line of the staged kut/tests.txt: name, flat file, smp,
// timeout seconds, check, qemu arguments.
type KUTTest struct {
	Name, File, Smp, Timeout, Check string
	Args                            []string
}

// ParseKUTManifest reads kut/tests.txt.
func ParseKUTManifest(b []byte) []KUTTest {
	var out []KUTTest
	for _, l := range strings.Split(string(b), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.SplitN(l, "\t", 6)
		if len(f) < 6 {
			continue
		}
		out = append(out, KUTTest{Name: f[0], File: f[1], Smp: f[2], Timeout: f[3], Check: f[4], Args: SplitQuoted(f[5])})
	}
	return out
}

// SplitQuoted splits words the way unittests.cfg values are written: blanks
// separate, and single or double quotes group without being kept.
func SplitQuoted(s string) []string {
	var out []string
	var cur strings.Builder
	in, quote, word := false, byte(0), false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case in && ch == quote:
			in = false
		case in:
			cur.WriteByte(ch)
		case ch == '\'' || ch == '"':
			in, quote, word = true, ch, true
		case ch == ' ' || ch == '\t':
			if word {
				out = append(out, cur.String())
				cur.Reset()
				word = false
			}
		default:
			cur.WriteByte(ch)
			word = true
		}
	}
	if word {
		out = append(out, cur.String())
	}
	return out
}

// KUTCheck is unittests.cfg's "check": every <path>=<value> must hold.  It
// returns the first one that does not, or "".
func KUTCheck(check string, read func(string) (string, error)) string {
	for _, c := range strings.Fields(check) {
		path, value, _ := strings.Cut(c, "=")
		got, err := read(path)
		if err != nil || strings.TrimSpace(got) != value {
			return c
		}
	}
	return ""
}

// KUTArgv is x86/run's qemu command line for one test, under its bound.
func KUTArgv(dir string, t KUTTest) []string {
	argv := []string{"timeout", "-k", "5", t.Timeout, "qemu-system-x86_64",
		"--no-reboot", "-nodefaults", "-global", "kvm-pit.lost_tick_policy=discard",
		"-device", "pc-testdev", "-device", "isa-debug-exit,iobase=0xf4,iosize=0x4",
		"-display", "none", "-serial", "stdio", "-device", "pci-testdev",
		"-machine", "accel=kvm", "-kernel", dir + "/" + t.File, "-smp", t.Smp}
	return append(argv, t.Args...)
}

// KUTVerdict classifies qemu's status.  A test ends by writing its status to
// isa-debug-exit, which exits qemu with (status << 1) | 1: 0 pass, 1 fail,
// and 77 >> 1 for skip, so that qemu's status is autotools' 77.  Anything
// else never reached the end.
func KUTVerdict(rc int) string {
	switch rc {
	case 1:
		return "pass"
	case 77:
		return "skip"
	case 3:
		return "fail"
	case 124, 137:
		return "timeout"
	}
	return "error"
}
