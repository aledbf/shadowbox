package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A Boot is one run-l1.sh invocation.
type Boot struct {
	Item    Item
	Vendor  string
	CPUs    int
	Rep     int
	Side    string // ab only: "a" or "b"
	LogName string
}

type Env struct {
	Testbed string
	Out     string
	Results string
	DryRun  bool
	summary *os.File
	metrics *os.File
}

func (e *Env) runL1() string { return filepath.Join(e.Testbed, "scripts", "run-l1.sh") }

// hostVariant is what out/build-host was last built as.
func (e *Env) hostVariant() string {
	cfg, err := os.ReadFile(filepath.Join(e.Out, "build-host", ".config"))
	if err != nil {
		return "unknown"
	}
	if strings.Contains(string(cfg), "\nCONFIG_KVM_PVM_STATS=y") {
		return "stats"
	}
	return "timing"
}

// Boots expands an item into the boots it takes, in the order they run.
func Boots(it Item) ([]Boot, error) {
	cpus, err := it.Ints("cpus", []int{0})
	if err != nil {
		return nil, err
	}
	reps, err := it.Ints("reps", []int{1})
	if err != nil {
		return nil, err
	}
	vendors := it.List("vendor")
	if vendors == nil {
		vendors = []string{"pvm"}
	}
	var out []Boot
	for rep := 1; rep <= reps[0]; rep++ {
		for _, c := range cpus {
			for _, v := range vendors {
				if it.Kind == "ab" {
					// Alternate the order every repetition, so slow
					// drift lands on both sides.
					sides := []string{"a", "b"}
					if rep%2 == 0 {
						sides = []string{"b", "a"}
					}
					for _, s := range sides {
						out = append(out, Boot{Item: it.Variant(s), Vendor: v, CPUs: c, Rep: rep, Side: s})
					}
					continue
				}
				out = append(out, Boot{Item: it, Vendor: v, CPUs: c, Rep: rep})
			}
		}
	}
	for i := range out {
		b := &out[i]
		name := b.Item.Name + "-" + b.Vendor
		if b.CPUs > 0 {
			name += fmt.Sprintf("-cpu%d", b.CPUs)
		}
		if reps[0] > 1 {
			name += fmt.Sprintf("-r%d", b.Rep)
		}
		b.LogName = name
	}
	return out, nil
}

// command builds the environment and arguments run-l1.sh takes.
func (e *Env) command(b Boot) (*exec.Cmd, string, error) {
	it := b.Item
	suite := it.Get("suite", "default")
	var env []string
	var guest []string
	if cases := it.List("cases"); cases != nil {
		// "|" joins them: the harness selects exactly those names.
		// A single name selects it in any suite by itself.
		if len(cases) == 1 {
			guest = append(guest, "pvmtest.only="+cases[0])
		} else {
			guest = append(guest, "pvmtest.only="+strings.Join(cases, "|"))
		}
	}
	if it.Get("stats", "") == "on" {
		guest = append(guest, "pvmtest.statsmsr=0x4b564d2f")
	}
	guest = append(guest, it.List("guest")...)
	if len(guest) > 0 {
		env = append(env, "GUEST_APPEND="+strings.Join(guest, ","))
	}
	if b.CPUs > 0 {
		env = append(env, fmt.Sprintf("GUEST_CPUS=%d", b.CPUs))
	}
	if m := it.List("mod"); m != nil {
		env = append(env, "MOD_ARGS="+strings.Join(m, ","))
	}
	var l1 []string
	if it.Get("pti", "") == "on" {
		l1 = append(l1, "pti=on")
	}
	switch it.Get("l1", "kvm") {
	case "kvm":
	case "tcg", "tcg-la57":
		cpu := "max"
		if it.Get("l1", "") == "tcg-la57" {
			cpu = "max,la57=on"
		}
		// Emulated: everything is an order of magnitude slower.
		env = append(env, "L1_ACCEL=tcg", "L1_CPU="+cpu, "L1_TIMEOUT=20000")
		l1 = append(l1, "pvmtest.guest_timeout=15000")
	default:
		return nil, "", fmt.Errorf("l1=%s: kvm, tcg or tcg-la57", it.Get("l1", ""))
	}
	l1 = append(l1, it.List("l1append")...)
	if len(l1) > 0 {
		env = append(env, "L1_APPEND="+strings.Join(l1, " "))
	}
	if p := it.Get("profile", ""); p != "" {
		suite = "profile"
		env = append(env, "PROFILE_CASE="+p)
	}
	suffix := "pvmtest-" + b.LogName
	env = append(env, "LOG_SUFFIX="+suffix)

	timeout := it.Get("timeout", "")
	if timeout == "" {
		timeout = "1500"
		if it.Get("l1", "kvm") != "kvm" {
			timeout = "21000"
		}
	}
	args := []string{"--foreground", "-k", "10", timeout, e.runL1(), suite, b.Vendor}
	cmd := exec.Command("timeout", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = e.Testbed
	// run-l1.sh names its log l1-<suite>-<vendor>-<suffix>.log.
	logSuite := suite
	log := filepath.Join(e.Out, "logs", fmt.Sprintf("l1-%s-%s-%s.log", logSuite, b.Vendor, suffix))
	return cmd, log, nil
}

// Status of one boot against what the item expects.
type Status struct {
	Boot    Boot
	Outcome *Outcome
	Verdict string // ok, FAIL
	Why     string
	Seconds float64
}

// judge decides a boot from its log.  checks are the verdicts of the
// testbed's own log checkers (sanitize-log.sh, check-selftests.sh), empty
// when they passed.
func judge(b Boot, o *Outcome, logText string, exitCode int, checks []string) (string, string) {
	it := b.Item
	if it.Get("expect", "pass") == "refuse-load" {
		refused := o.LoadFailed || o.FailClosed == "ok"
		if !refused {
			return "FAIL", "the module loaded; expected it to refuse"
		}
		if r := it.Get("reason", ""); r != "" {
			re, err := regexp.Compile("(?i)" + r)
			if err != nil {
				return "FAIL", "reason: " + err.Error()
			}
			if !re.MatchString(logText) {
				return "FAIL", "refused, but nothing in the log says /" + r + "/"
			}
		}
		if len(checks) > 0 {
			return "FAIL", "refused, but " + checks[0]
		}
		return "ok", "module refused to load, as expected"
	}
	if o.LoadFailed {
		return "FAIL", "the vendor module did not load"
	}
	unexpected := o.Unexpected(it.List("allow-fail"))
	if len(unexpected) > 0 {
		return "FAIL", "failed: " + strings.Join(unexpected, " ")
	}
	if o.HostFail > 0 {
		return "FAIL", fmt.Sprintf("%d host test(s) failed", o.HostFail)
	}
	if len(o.Passed) == 0 && o.HostPass == 0 && len(o.Metrics) == 0 {
		return "FAIL", fmt.Sprintf("no results in the log (run-l1 exit %d)", exitCode)
	}
	if len(checks) > 0 {
		return "FAIL", checks[0]
	}
	why := fmt.Sprintf("%d passed", len(o.Passed)+o.HostPass)
	if len(o.Failed) > 0 {
		why += fmt.Sprintf(", %d allowed failure(s): %s", len(o.Failed), strings.Join(o.Failed, " "))
	}
	return "ok", why
}

// logChecks runs the testbed's own checkers on a finished log.
func (e *Env) logChecks(b Boot, log string) []string {
	var fails []string
	run := func(what string, args ...string) {
		cmd := exec.Command(filepath.Join(e.Testbed, "scripts", args[0]), args[1:]...)
		cmd.Dir = e.Testbed
		if out, err := cmd.CombinedOutput(); err != nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			fails = append(fails, what+": "+lines[len(lines)-1])
		}
	}
	run("sanitize-log.sh", "sanitize-log.sh", "-q", log)
	if b.Item.Get("suite", "") == "hosttests" {
		run("check-selftests.sh", "check-selftests.sh", log, b.Vendor)
	}
	return fails
}

func (e *Env) runBoot(b Boot) (*Status, error) {
	cmd, log, err := e.command(b)
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(e.Results, "logs", b.LogName+".log")
	if e.DryRun {
		fmt.Printf("  %s\n    %s %s\n", b.LogName, strings.Join(cmd.Env[len(os.Environ()):], " "),
			strings.Join(cmd.Args, " "))
		return &Status{Boot: b, Outcome: &Outcome{}, Verdict: "dry"}, nil
	}
	start := time.Now()
	out, _ := os.Create(filepath.Join(e.Results, "logs", b.LogName+".run-l1.out"))
	cmd.Stdout, cmd.Stderr = out, out
	runErr := cmd.Run()
	out.Close()
	code := 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if err := os.Rename(log, dest); err != nil {
		return &Status{Boot: b, Outcome: &Outcome{}, Verdict: "FAIL",
			Why: fmt.Sprintf("no log at %s (run-l1 exit %d)", log, code)}, nil
	}
	raw, err := os.ReadFile(dest)
	if err != nil {
		return nil, err
	}
	o := ParseLog(strings.NewReader(string(raw)))
	verdict, why := judge(b, o, string(raw), code, e.logChecks(b, dest))
	return &Status{Boot: b, Outcome: o, Verdict: verdict, Why: why,
		Seconds: time.Since(start).Seconds()}, nil
}

func (e *Env) record(s *Status) {
	b := s.Boot
	fmt.Fprintf(e.summary, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%.0f\t%s\n", b.LogName, b.Item.Name,
		b.Vendor, b.CPUs, b.Rep, b.Side, s.Verdict, s.Seconds, s.Why)
	for _, m := range s.Outcome.Metrics {
		fmt.Fprintf(e.metrics, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%g\t%s\n", b.LogName, b.Item.Name,
			b.Vendor, b.CPUs, b.Rep, b.Side, m.Name, m.Value, m.Unit)
	}
	if pairs, keys := s.Outcome.StatsPairs(); len(pairs) > 0 {
		writeStats(filepath.Join(e.Results, "stats-"+b.LogName+".tsv"), pairs, keys)
	}
}

func writeStats(path string, pairs []StatsPair, keys []string) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprint(f, "counter")
	for _, p := range pairs {
		fmt.Fprintf(f, "\t%s", p.Name)
	}
	fmt.Fprintln(f)
	for _, k := range keys {
		fmt.Fprint(f, k)
		for _, p := range pairs {
			fmt.Fprintf(f, "\t%d", p.Deltas[k])
		}
		fmt.Fprintln(f)
	}
}

func writeFile(path, text string) error { return os.WriteFile(path, []byte(text), 0o644) }

func gitDescribe(dir string) string {
	out, err := exec.Command("git", "-C", dir, "describe", "--always", "--dirty").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func atoiDef(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
