package main

// The agent proper, in the order the bash agent it replaced did things.
//
// Runs inside L1, as the PVM host.  Loads kvm-pvm, states plainly whether
// it came up, then boots the guest under it and forwards the guest's
// serial output to L1's console -- which is L0's log file.
//
// Every line it prints is prefixed so the two kernels' output can be told
// apart in a single log.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type agent struct {
	p          Params
	vendor     string
	suite      string // after ResolveMode
	mode       string
	t          string   // tracefs
	perfPrefix []string // $PERF_PREFIX, word split
}

// toConsole is `exec >/dev/console 2>&1`.
//
// Straight to the console.  Routed through journald, the console output is
// rate limited, and what gets dropped is the end -- which is where the
// diagnostics are.
func toConsole() {
	f, err := os.OpenFile("/dev/console", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		// bash reports a failed exec redirection and carries on with the
		// descriptors it had.
		shErr("/dev/console: " + errText(err))
		return
	}
	syscall.Dup3(int(f.Fd()), 1, 0)
	syscall.Dup3(int(f.Fd()), 2, 0)
	f.Close()
}

func dmesg() []byte { return capture([]string{"dmesg"}, stderrConsole) }

func runAgent() int {
	toConsole()

	var uts syscall.Utsname
	syscall.Uname(&uts)
	say("kernel: " + utsString(uts.Release[:]))
	cpuinfo, _ := os.ReadFile("/proc/cpuinfo")
	say("cpu: " + cpuModel(cpuinfo))

	cmdline, _ := os.ReadFile("/proc/cmdline")
	a := &agent{p: ParseCmdline(string(cmdline))}
	a.vendor = a.p.Vendor

	// Which vendor to run under.  Both are modules and both travel on the
	// payload share, because the rootfs is bootstrapped once and the kernel
	// version string -- and so the path modprobe would search -- moves
	// with every commit.  insmod by path sidesteps that entirely.
	//
	// The suite is read early: the "no /dev/kvm" bail below is a failure
	// for every suite except the one whose whole point is that the module
	// refuses.
	mod, ok := ModulePath(a.vendor)
	if !ok {
		say("unknown vendor: " + a.vendor)
		poweroff()
		// Had poweroff returned, the bash's next line used the unset
		// $mod, and "set -u" ended the script there.
		shErr("mod: unbound variable")
		return 1
	}

	say("loading " + a.vendor + " from " + mod + " " + a.p.ModArgs)
	// insmod's own status.  The bash tested the status of the sed at the end
	// of "insmod ... | sed" instead, which is always 0, so "L1: insmod
	// failed" -- what tools/pvmtest reads as the module not loading -- was
	// never printed.
	if rc := streamPrefixed(InsmodArgv(mod, a.p.ModArgs), stderrMerged, "L1: insmod: ", -1); rc != 0 {
		say("insmod failed")
	}

	filterPrefixed([]string{"lsmod"}, stderrConsole,
		func(l []byte) bool { return bytes.HasPrefix(l, []byte("kvm")) }, -1, "L1: lsmod: ")

	writePrefixed(os.Stdout, "L1: dmesg: ", DmesgKVM(dmesg(), 40))

	if a.p.Suite == "failclosed" {
		a.failClosed()
	}

	if !exists("/dev/kvm") {
		say("no /dev/kvm -- the PVM host did not register")
		echo("PVMTEST-RESULT: fail stage=1 reason=no-kvm-device")
		poweroff()
	}
	say("/dev/kvm present")
	meltdown, _ := os.ReadFile("/sys/devices/system/cpu/vulnerabilities/meltdown")
	say("host PTI: " + stripNewlines(string(meltdown)))
	// $(grep -o ' pti' /proc/cpuinfo | head -1 || echo 'not set'): the
	// status is head's, so "not set" never appears -- an empty value does.
	pti := ""
	if bytes.Contains(cpuinfo, []byte(" pti")) {
		pti = " pti"
	}
	say("host pti flag: " + pti)
	thp, err := os.ReadFile("/sys/kernel/mm/transparent_hugepage/enabled")
	if err != nil {
		say("host THP: not built")
	} else {
		say("host THP: " + stripNewlines(string(thp)))
	}
	paging := "4-level"
	if bytes.Contains(cpuinfo, []byte(" la57")) {
		paging = "5-level"
	}
	say("host paging: " + paging)

	// The payload share is already mounted: the stub in the image did it
	// before exec'ing this program, which is how this program got here at
	// all.  Mounting it a second time fails with "no channels available
	// for device payload", and the agent used to treat that as fatal.

	a.mode, a.suite = ResolveMode(a.p.Suite)

	if a.mode == modeHosttests {
		a.hostTests()
	}
	if a.mode == modeKUT {
		a.kutTests()
	}

	say("qemu: " + a.qemuVersion())
	streamPrefixed([]string{"ls", "-l", payload}, stderrConsole, "L1: payload: ", -1)

	a.t = "/sys/kernel/tracing"
	if !isDir(a.t) {
		a.t = "/sys/kernel/debug/tracing"
	}
	if isDir(a.t) {
		for _, w := range TraceSetup(a.mode, a.suite) {
			if echoTo(filepath.Join(a.t, w.File), w.Data) == nil && w.Say != "" {
				say(w.Say)
			}
		}
	}

	guestTimeout := GuestTimeout(a.suite, a.p.GuestTimeout)
	say("guest: " + a.p.GuestCPUs + " vcpus, " + a.p.GuestMem + ", timeout " + guestTimeout + "s")
	appendLine := GuestAppend(a.p, a.mode, a.suite)

	// Core dumps off, on purpose.  vfs_coredump() is what sleeps, and it
	// is sleeping with a leaked preempt count -- so the dump never
	// finishes, qemu never exits, and the agent waits on it forever.
	// Without a dump the SIGSEGV just kills qemu and we get to keep
	// debugging.
	//
	// print-fatal-signals gives the faulting address, RIP and error code
	// for the killed process, which is the one line that says whether this
	// is qemu faulting or a PVM guest fault reaching the host's own #PF
	// handler.
	//
	// bash's "ulimit -c 0" lowers the hard limit along with the soft one.
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		shErr("ulimit: core file size: cannot modify limit: " + errText(err))
	}
	echoTo("/proc/sys/kernel/core_pattern", "core\n")
	echoTo("/proc/sys/kernel/print-fatal-signals", "1\n")

	// For the perf suite, count the guest's exits by reason while it runs.
	// This is the whole point of the pvm_trace.h port: "HYPERCALL" and
	// "SYSCALL" as bare totals do not say where the time goes.
	//
	// The payload carries the libraries L1 itself does not have.
	if v, ok := os.LookupEnv("LD_LIBRARY_PATH"); ok && v != "" {
		os.Setenv("LD_LIBRARY_PATH", payload+":"+v)
	} else {
		os.Setenv("LD_LIBRARY_PATH", payload)
	}
	a.setupPerf()

	a.runGuest(guestTimeout, appendLine, "q35", "-machine", "q35,accel=kvm")

	a.traceReports()
	a.perfReports()

	if isDir(a.t) {
		echoTo(filepath.Join(a.t, "tracing_on"), "0\n")
	}

	a.dmesgReports()

	// The teardown thread and the mmu-notifier unmap storm are the loudest
	// things in the buffer and say nothing.  Everything else, in order, is
	// what actually happened -- and picking a thread by name gets it
	// wrong, because the qemu IO thread and the vCPU threads share it.
	if isDir(a.t) {
		say("--- last 70 trace lines, teardown and unmaps removed ---")
		// grep reports an unreadable file on stderr and prints nothing.
		if f, err := os.Open(filepath.Join(a.t, "trace")); err != nil {
			os.Stderr.WriteString("grep: " + filepath.Join(a.t, "trace") + ": " + errText(err) + "\n")
		} else {
			writePrefixed(os.Stdout, "L1: kvm: ", LastTraceLines(f, 70))
			f.Close()
		}
	}

	say("done")
	return poweroff()
}

func utsString(b []int8) string {
	s := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		s = append(s, byte(c))
	}
	return string(s)
}

// cpuModel is $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2-).
func cpuModel(cpuinfo []byte) string {
	for _, l := range splitLines(cpuinfo) {
		if strings.Contains(l, "model name") {
			return cutF2(l)
		}
	}
	return ""
}

// failClosed is suite=failclosed.
//
// The module is expected to refuse.  What is checked is that it says why,
// that it leaves no /dev/kvm behind, and that the host is still healthy --
// a refusal that half-registered would be worse than a panic, because
// nothing would notice.
func (a *agent) failClosed() {
	say("=== expecting " + a.vendor + " to refuse to load ===")
	lsmod := capture([]string{"lsmod"}, stderrConsole)
	stayed := false
	for _, l := range splitLines(lsmod) {
		if strings.HasPrefix(l, "kvm_pvm") {
			stayed = true
			break
		}
	}
	switch {
	case stayed:
		echo("FAILCLOSED: fail reason=module-stayed-loaded")
	case exists("/dev/kvm"):
		echo("FAILCLOSED: fail reason=kvm-device-present")
	default:
		echo("FAILCLOSED: ok reason=refused")
	}
	say("--- what it said ---")
	writePrefixed(os.Stdout, "L1: dmesg: ", DmesgKVM(dmesg(), 20))
	poweroff()
	// The bash carried on past a poweroff that returned, and so does this.
}

// hostTests is suite=hosttests.
func (a *agent) hostTests() {
	// pvmtest.test_timeout=<s>: per-program bound, for an L1 under TCG
	// where a case that takes seconds on hardware takes many minutes.
	tt := a.p.TestTimeout
	rc := 0
	found := false
	for _, t := range globAll(payload + "/hosttests") {
		if !executable(t) {
			continue
		}
		found = true
		say("=== " + filepath.Base(t) + " ===")
		// Unbuffered through sed so a test that wedges still shows what it
		// managed to print.
		r := streamPrefixed(TestArgv(tt, t), stderrMerged, "H: ", -1)
		say(filepath.Base(t) + " exited " + strconv.Itoa(r))
		if r != 0 {
			rc = 1
		}
	}
	// The kernel's own KVM selftests, if any were staged.  Their verdict
	// is not folded into rc: they build a guest that expects CPL0 and its
	// own page tables, so what they do under PVM is a question rather
	// than an assertion.  The outcome of each is reported by name and
	// classified outside, against configs/kvm-selftests-expect.txt.
	//
	// Exit codes are the kselftest convention: 0 pass, 4 skip, anything
	// else a failure.
	for _, t := range globAll(payload + "/kvm-selftests") {
		if !executable(t) {
			continue
		}
		found = true
		name := SelftestName(filepath.Base(t))
		r := 1
		if f, err := os.OpenFile("/tmp/st.log", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666); err != nil {
			shErr("/tmp/st.log: " + errText(err))
		} else {
			r = run(TestArgv(tt, t), os.Stdin, f, f)
			f.Close()
		}
		verdict := SelftestVerdict(r)
		echo("SELFTEST: " + name + " " + verdict + " rc=" + strconv.Itoa(r))
		// Only the tail, and only when it did not pass: a passing
		// selftest's output is pages of nothing anyone will read.
		if verdict != "pass" {
			if b, err := os.ReadFile("/tmp/st.log"); err != nil {
				os.Stderr.WriteString("tail: cannot open '/tmp/st.log' for reading: " + errText(err) + "\n")
			} else {
				prefixStream(os.Stdout, "S["+name+"]: ", bytes.NewReader(tailBytes(b, 15)), -1)
			}
		}
	}

	switch {
	case !found:
		say("no host tests in the payload")
		echo("PVMTEST-RESULT: fail stage=hosttests reason=no-tests")
	case rc == 0:
		echo("PVMTEST-RESULT: ok stage=hosttests")
	default:
		echo("PVMTEST-RESULT: fail stage=hosttests")
	}
	prefixStream(os.Stdout, "L1: dmesg: ", bytes.NewReader(tailBytes(dmesg(), 60)), -1)
	poweroff()
	// As in the bash, a poweroff that returns falls through to the guest.
}

// kutTests is suite=kut: kvm-unit-tests against the loaded vendor module,
// one qemu each.  Each outcome is reported by name and judged outside,
// against configs/kut-expect.txt.
func (a *agent) kutTests() {
	dir := payload + "/kut"
	b, err := os.ReadFile(dir + "/tests.txt")
	tests := ParseKUTManifest(b)
	if err != nil || len(tests) == 0 {
		say("no kvm-unit-tests in the payload")
		echo("PVMTEST-RESULT: fail stage=kut reason=no-tests")
		poweroff()
		return
	}
	for _, p := range []string{"ept", "unrestricted_guest", "enable_shadow_vmcs"} {
		v, err := os.ReadFile("/sys/module/kvm_intel/parameters/" + p)
		if err == nil {
			say("kvm_intel " + p + ": " + stripNewlines(string(v)))
		}
	}
	read := func(p string) (string, error) {
		v, err := os.ReadFile(p)
		return string(v), err
	}
	for _, t := range tests {
		if c := KUTCheck(t.Check, read); c != "" {
			echo("KUT: " + t.Name + " skip rc=0 check=" + c)
			continue
		}
		r := 1
		if f, err := os.OpenFile("/tmp/kut.log", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666); err != nil {
			shErr("/tmp/kut.log: " + errText(err))
		} else {
			r = run(KUTArgv(dir, t), nil, f, f)
			f.Close()
		}
		log, _ := os.ReadFile("/tmp/kut.log")
		verdict := KUTVerdict(r)
		summary := ""
		for _, l := range splitLines(log) {
			if strings.HasPrefix(l, "SUMMARY: ") {
				summary = " " + strings.TrimPrefix(l, "SUMMARY: ")
			}
		}
		echo("KUT: " + t.Name + " " + verdict + " rc=" + strconv.Itoa(r) + summary)
		if verdict != "pass" && verdict != "skip" {
			// The FAIL lines say which subtests; the tail says where it
			// stopped when it never reached a summary.
			for _, l := range splitLines(log) {
				if strings.HasPrefix(l, "FAIL: ") {
					echo("K[" + t.Name + "]: " + l)
				}
			}
			prefixStream(os.Stdout, "K["+t.Name+"]: ", bytes.NewReader(tailBytes(log, 10)), -1)
		}
	}
	echo("PVMTEST-RESULT: ok stage=kut")
	prefixStream(os.Stdout, "L1: dmesg: ", bytes.NewReader(tailBytes(dmesg(), 40)), -1)
	poweroff()
}

// globAll is the list "dir/*" expands to: no dot files, sorted by bytes (C
// collation).  A directory that does not exist or is empty leaves the
// unmatched pattern, which the -x test then skips.
func globAll(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// qemuVersion is $(qemu-system-x86_64 -version 2>&1 | head -1).
func (a *agent) qemuVersion() string {
	r, wait := pipe([]string{"qemu-system-x86_64", "-version"}, stderrMerged)
	var first string
	if r != nil {
		eachLine(r, func(l []byte, _ bool) bool {
			first = string(l)
			return false
		})
		r.Close()
	}
	wait()
	return first
}

// setupPerf decides $PERF_PREFIX, with the side effects each choice has.
func (a *agent) setupPerf() {
	cdTmp := func() {
		if err := os.Chdir("/tmp"); err != nil {
			shErr("cd: /tmp: " + errText(err))
			os.Exit(1)
		}
	}
	k := PerfKind(a.mode, a.suite, executable(perfPath))
	switch k {
	case perfMMU:
		cdTmp()
		echoLoud("/proc/sys/kernel/perf_event_paranoid", "-1\n")
		say("counting exits and shadow MMU events")
		a.perfPrefix = PerfPrefix(k)
	case perfLock:
		cdTmp()
		echoLoud("/proc/sys/kernel/perf_event_paranoid", "-1\n")
		echoLoud("/proc/sys/kernel/kptr_restrict", "0\n")
		say("recording lock contention")
		a.perfPrefix = PerfPrefix(k)
	case perfProfile:
		cdTmp()
		echoLoud("/proc/sys/kernel/kptr_restrict", "0\n")
		echoLoud("/proc/sys/kernel/perf_event_paranoid", "-1\n")
		say("sampling qemu's cycles at 4kHz")
		a.perfPrefix = PerfPrefix(k)
	case perfKVMStat:
		// "perf kvm stat" is compiled out entirely without
		// libtraceevent, and a perf built that way answers the subcommand
		// with its own usage text -- which, wrapped around qemu, silently
		// costs the whole run.  Ask first.  (In the current directory,
		// which is still "/", as it was for the bash.)
		if run(KVMStatProbeArgv(), os.Stdin, nil, nil) == 0 {
			say("recording kvm exits with perf")
			// perf writes perf.data.kvm into the current directory, and
			// the unit starts in "/", which is not somewhere to leave
			// files.
			cdTmp()
			a.perfPrefix = PerfPrefix(k)
		} else {
			say("perf has no working 'kvm stat' (built without libtraceevent?)")
		}
	}
}

// runGuest is run_guest(): one boot of the guest on one machine type.
//
// Two machine types, because they differ in exactly the way that matters.
//
// q35 runs SeaBIOS first, in real mode, and SeaBIOS enables interrupts --
// at which point the host has to inject an IRQ into a guest that is not in
// PVM mode yet.  do_pvm_event() does not support that: it warns and raises
// a triple fault, which is the KVM_EXIT_SHUTDOWN we see.
//
// microvm has no firmware.  The kernel is entered directly, with
// interrupts off, and stays that way until it is in long mode -- so
// nothing is ever injected in non-PVM mode.  If this one gets further,
// that confirms where the problem is.
func (a *agent) runGuest(guestTimeout, appendLine, tag string, machine ...string) {
	os.Remove("/tmp/guest.log")
	os.Remove("/tmp/qemu.log")
	say("=== booting the PVM guest on " + tag + " ===")
	// Bounded, so that a guest that wedges its vCPU thread does not take
	// the whole agent down with it and cost us the host side diagnostics.
	// The bound has to fit the suite: the stress and perf cases allow
	// themselves several minutes each, and 45s was chosen back when the
	// guest was dying in under a second.
	argv := QemuArgv(guestTimeout, a.perfPrefix, machine, a.p.GuestCPUs, a.p.GuestMem, appendLine)
	rc := 1
	if f, err := os.OpenFile("/tmp/qemu.log", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666); err != nil {
		shErr("/tmp/qemu.log: " + errText(err))
	} else {
		rc = run(argv, nil /* < /dev/null */, f, f)
		f.Close()
	}
	say(tag + ": qemu exited " + strconv.Itoa(rc))
	if nonEmpty("/tmp/qemu.log") {
		prefixFile("/tmp/qemu.log", "L1: "+tag+" qemu: ")
	}
	if nonEmpty("/tmp/guest.log") {
		prefixFile("/tmp/guest.log", "G["+tag+"]: ")
	} else {
		say(tag + ": the guest produced no serial output at all")
	}
}

// prefixFile is sed 's/^/prefix/' <file>.
func prefixFile(path, prefix string) {
	f, err := os.Open(path)
	if err != nil {
		os.Stderr.WriteString("sed: can't read " + path + ": " + errText(err) + "\n")
		return
	}
	prefixStream(os.Stdout, prefix, f, -1)
	f.Close()
}

// withTrace runs fn over a fresh read of $T/trace, as each of the bash's
// sed/grep passes opened it anew -- tracing is still on while they run.
func (a *agent) withTrace(fn func(io.Reader)) {
	p := filepath.Join(a.t, "trace")
	f, err := os.Open(p)
	if err != nil {
		os.Stderr.WriteString("sed: can't read " + p + ": " + errText(err) + "\n")
		fn(bytes.NewReader(nil))
		return
	}
	defer f.Close()
	fn(f)
}

// traceReports: which instructions the host is emulating, and how often.
// Every #GP the guest takes for a privileged instruction with no paravirt
// hook lands in the emulator, and the exit histogram counts them all as
// one row.
func (a *agent) traceReports() {
	if !((a.suite == "perf" || a.suite == "mmu") && isDir(a.t)) {
		return
	}
	// What the exits were.  This is the first question about a hypervisor
	// and the counters cannot answer it: kvm:kvm_hypercall fires only in
	// kvm_emulate_hypercall(), which PVM reaches for the KVM-specific
	// hypercalls alone, so every PVM_HC_* lands here as a bare exit with
	// no counter of its own.  The reason field of kvm_exit has it -- PVM
	// fills it in from pvm_get_syscall_exit_reason() -- so count that.
	say("--- exits by reason ---")
	a.withTrace(func(r io.Reader) { writePrefixed(os.Stdout, "L1: exit: ", ExitsByReason(r)) })

	// Which guest code is causing them.  The reason says what the exit
	// was; this says who asked for it, which is the part you can do
	// something about.  Raw RIPs: the guest kernel is PIE and relocated,
	// so resolving them needs its vmlinux and its runtime _text, both of
	// which live outside L1.  pvmtest resolve-exits does that half.
	say("--- exits by reason and guest rip ---")
	a.withTrace(func(r io.Reader) { writePrefixed(os.Stdout, "L1: exitrip: ", ExitsByRip(r)) })

	// $(grep -c kvm_emulate_insn "$T/trace" 2>/dev/null || echo 0): with no
	// match grep prints its "0" and fails, and the echo adds a second one.
	total := "0"
	if f, err := os.Open(filepath.Join(a.t, "trace")); err == nil {
		if c := CountLines(f, "kvm_emulate_insn"); c == 0 {
			total = "0\n0"
		} else {
			total = strconv.Itoa(c)
		}
		f.Close()
	}
	say("--- emulated instructions: " + total + " traced ---")
	a.withTrace(func(r io.Reader) { writePrefixed(os.Stdout, "L1: insn: ", InsnHistogram(r)) })

	// The list above is dominated by the bootstrap: SeaBIOS runs fully
	// emulated in non-PVM mode, up to 130 instructions per exit, so a
	// memcpy loop drowns out everything else.  What causes the #GP exits
	// is the privileged subset, so count that separately.
	say("--- privileged instructions only ---")
	a.withTrace(func(r io.Reader) { writePrefixed(os.Stdout, "L1: priv: ", PrivHistogram(r)) })

	// Which MSRs, since wrmsr is what is left once the console is quiet.
	say("--- MSR accesses by register ---")
	a.withTrace(func(r io.Reader) { writePrefixed(os.Stdout, "L1: msr: ", MSRHistogram(r)) })

	say("--- dropped by the trace buffer ---")
	if b, err := os.ReadFile(filepath.Join(a.t, "per_cpu/cpu0/stats")); err == nil {
		writePrefixed(os.Stdout, "L1: insn: cpu0 ", grepLines(b, func(l string) bool {
			return strings.Contains(l, "overrun")
		}))
	}
}

// perfReports reads back what the perf wrapper recorded.
func (a *agent) perfReports() {
	if a.mode == modeMMU && nonEmpty("/tmp/mmu.txt") {
		say("--- shadow MMU event counts ---")
		b, _ := os.ReadFile("/tmp/mmu.txt")
		writePrefixed(os.Stdout, "L1: mmu: ", MMUCounts(b))
	}

	if a.mode == modeLock && nonEmpty("/tmp/lock.data") {
		say("--- lock contention by lock, wait_total ---")
		streamPrefixed(lockContentionArgv("-E", "12", "-F", "contended,wait_total,wait_max,avg_wait"),
			stderrMerged, "L1: lock: ", -1)
		say("--- lock contention by caller, wait_total ---")
		streamPrefixed(lockContentionArgv("-E", "12", "-l", "-F", "contended,wait_total,avg_wait"),
			stderrMerged, "L1: lockaddr: ", -1)
		say("--- contended in the shadow MMU page fault path ---")
		streamPrefixed(lockContentionArgv("-E", "8", "-S", "kvm_mmu_page_fault", "-F", "contended,wait_total,wait_max,avg_wait"),
			stderrMerged, "L1: lockpf: ", -1)
	}

	if a.mode == modeProfile && nonEmpty("/tmp/cycles.data") {
		// grep -vE "^#|^$"
		report := func(l []byte) bool { return len(l) > 0 && l[0] != '#' }
		say("--- host cycles, kernel symbols ---")
		filterPrefixed(profileReportArgv("--percent-limit", "0.4", "-g", "none"),
			stderrDiscard, report, 28, "L1: prof: ")

		// Self time, which is where a lock's spinning shows: with
		// children, a slow path is buried under every caller that reached
		// it.
		say("--- host cycles, self time ---")
		filterPrefixed(profileReportArgv("--no-children", "--percent-limit", "0.3", "-g", "none"),
			stderrDiscard, report, 40, "L1: self: ")

		say("--- anything with 'switcher' in the name ---")
		filterPrefixed(profileReportArgv("-g", "none"), stderrDiscard,
			func(l []byte) bool { return containsFold(string(l), "switcher") }, 10, "L1: prof: ")
	}

	if len(a.perfPrefix) > 0 && a.mode != modeProfile && a.mode != modeLock {
		// The name depends on whether perf recorded host, guest or both:
		// get_filename_for_perf_kvm() picks between .host, .guest and
		// .kvm.
		data := ""
		for _, f := range []string{"/tmp/perf.data.guest", "/tmp/perf.data.kvm", "/tmp/perf.data.host"} {
			if nonEmpty(f) {
				data = f
				break
			}
		}
		if data != "" {
			say("--- kvm exits by reason (" + filepath.Base(data) + ") ---")
			streamPrefixed(kvmStatReportArgv(data), stderrMerged, "L1: perf: ", 45)
		} else {
			say("perf produced no data file")
		}
	}
}

// dmesgReports is what the host kernel said about the guest run.
func (a *agent) dmesgReports() {
	// do_pvm_event() warns once per rate-limit window when the VMM injects
	// an event while the vCPU is still in the non-PVM bootstrap mode.  Its
	// presence or absence says whether that path was reached at all.
	say("--- what the host said about the guest ---")
	writePrefixed(os.Stdout, "L1: pvm: ", DmesgPVM(dmesg()))
	// The bash's `|| say "(nothing)"` tested sed's status, always 0: never
	// printed.
	say("--- injection warnings ---")
	writePrefixed(os.Stdout, "L1: warn: ", DmesgWarn(dmesg()))
	// Likewise "no 'non-PVM mode' warning": `if ! ... | sed` is never true.

	// The backtrace is the thing.  Print the region around it and nothing
	// else: a full dmesg dump is long enough that the tail of it is what
	// gets lost, and the tail is the part that matters.
	// The most informative line of all, if qemu died: the kernel logs the
	// faulting address, instruction pointer and error code for an
	// unhandled user signal.  A PVM guest runs at hardware CPL3 inside the
	// qemu thread, so a guest fault that reaches the host's own #PF
	// handler looks exactly like qemu faulting -- and this line is what
	// tells the two apart.
	say("--- did qemu fault, and where ---")
	writePrefixed(os.Stdout, "L1: sig: ", DmesgSig(dmesg()))
	// "no segfault reported": never printed, for the same reason.

	say("--- kernel complaints from the guest run ---")
	writePrefixed(os.Stdout, "L1: bug: ", DmesgBug(dmesg()))
	// "no BUG, GPF or fault in dmesg": never printed, for the same reason.
}
