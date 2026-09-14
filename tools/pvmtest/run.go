package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A Boot is one boot of L1.
type Boot struct {
	Item    Item
	Vendor  string
	CPUs    int
	Rep     int
	Side    string // ab only: "a" or "b"
	LogName string
}

type Env struct {
	Config  Config
	Testbed string
	Out     string
	Results string
	DryRun  bool
	summary *os.File
	metrics *os.File
}

// outFor is the out/ directory of an item's image set.
func (e *Env) outFor(it Item) string {
	if k := it.Get("kernel", ""); k != "" {
		return filepath.Join(e.Out, "refs", k)
	}
	return e.Out
}

// hostVariant is what an image set's host was last built as.
func (e *Env) hostVariant(out string) string {
	if raw, err := os.ReadFile(filepath.Join(out, "built")); err == nil {
		if f := strings.Fields(string(raw)); len(f) == 2 {
			return f[1]
		}
	}
	cfg, err := os.ReadFile(filepath.Join(out, "build-host", ".config"))
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
	vendorsOf := func(it Item) []string {
		if v := it.List("vendor"); v != nil {
			return v
		}
		return []string{"pvm"}
	}
	var out []Boot
	for rep := 1; rep <= reps[0]; rep++ {
		for _, c := range cpus {
			if it.Kind == "ab" {
				// Alternate the order every repetition, so slow drift
				// lands on both sides.  Each side may set its own
				// vendor and kernel.
				sides := []string{"a", "b"}
				if rep%2 == 0 {
					sides = []string{"b", "a"}
				}
				for _, s := range sides {
					v := it.Variant(s)
					for _, vendor := range vendorsOf(v) {
						out = append(out, Boot{Item: v, Vendor: vendor, CPUs: c, Rep: rep, Side: s})
					}
				}
				continue
			}
			for _, vendor := range vendorsOf(it) {
				out = append(out, Boot{Item: it, Vendor: vendor, CPUs: c, Rep: rep})
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

// l1Opts is what BootL1 needs for one boot of an item.
func (e *Env) l1Opts(b Boot) (L1Opts, error) {
	it := b.Item
	o := L1Opts{Suite: it.Get("suite", "default"), Vendor: b.Vendor}
	if cases := it.List("cases"); cases != nil {
		// "|" joins them: the harness selects exactly those names.  A
		// single name selects it in any suite by itself.
		o.Guest = append(o.Guest, "pvmtest.only="+strings.Join(cases, "|"))
	}
	if it.Get("stats", "") == "on" {
		o.Guest = append(o.Guest, "pvmtest.statsmsr=0x4b564d2f")
	}
	o.Guest = append(o.Guest, it.List("guest")...)
	if b.CPUs > 0 {
		o.CPUs = strconv.Itoa(b.CPUs)
	}
	o.Mod = it.List("mod")
	if it.Get("pti", "") == "on" {
		o.L1Args = append(o.L1Args, "pti=on")
	}
	switch it.Get("l1", "kvm") {
	case "kvm":
	case "tcg", "tcg-la57":
		// Emulated: everything is an order of magnitude slower.  TCG
		// emulates PCID and INVPCID, which kvm-pvm requires, but "max"
		// does not enable them.
		o.Accel, o.CPU = "tcg", "max,+pcid,+invpcid"
		if it.Get("l1", "") == "tcg-la57" {
			o.CPU += ",la57=on"
		}
		o.L1Args = append(o.L1Args, "pvmtest.guest_timeout=15000")
		o.Timeout = 20000 * time.Second
	default:
		return o, fmt.Errorf("l1=%s: kvm, tcg or tcg-la57", it.Get("l1", ""))
	}
	o.L1Args = append(o.L1Args, it.List("l1append")...)
	if p := it.Get("profile", ""); p != "" {
		o.Suite, o.Profile = "profile", p
	}
	if t := it.Get("timeout", ""); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil {
			return o, fmt.Errorf("timeout=%s: seconds", t)
		}
		o.Timeout = time.Duration(n) * time.Second
	}
	o.QMP = os.Getenv("L1_QMP")
	o.Debug = os.Getenv("L1_QEMU_DEBUG")
	return o, nil
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
// testbed's log checkers (Sanitize, CheckSelftests), empty
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
	if len(o.Passed) == 0 && o.HostPass == 0 && o.Selftests == 0 && o.KUT == 0 && len(o.Metrics) == 0 {
		return "FAIL", fmt.Sprintf("no results in the log (run-l1 exit %d)", exitCode)
	}
	if len(checks) > 0 {
		return "FAIL", checks[0]
	}
	why := fmt.Sprintf("%d passed", len(o.Passed)+o.HostPass)
	if o.Selftests > 0 {
		why += fmt.Sprintf(", %d selftests as expected", o.Selftests)
	}
	if o.KUT > 0 {
		why = fmt.Sprintf("%d kvm-unit-tests as expected (%s, %s)", o.KUT, kutConfig(it), kutSummary(logText))
	}
	if len(o.Failed) > 0 {
		why += fmt.Sprintf(", %d allowed failure(s): %s", len(o.Failed), strings.Join(o.Failed, " "))
	}
	return "ok", why
}

// logChecks runs the testbed's log checkers on a finished log.
func (e *Env) logChecks(b Boot, log string) []string {
	if !fileExists(log) {
		return nil
	}
	var fails []string
	if err := Sanitize(e.Config, []string{log}, true); err != nil {
		fails = append(fails, "sanitize: "+err.Error())
	}
	switch b.Item.Get("suite", "") {
	case "hosttests":
		if err := CheckSelftests(e.Config, log, b.Vendor); err != nil {
			fails = append(fails, "selftests: "+err.Error())
		}
	case "kut":
		if err := CheckKUT(e.Config, log, kutConfig(b.Item)); err != nil {
			fails = append(fails, "kvm-unit-tests: "+err.Error())
		}
	}
	return fails
}

func (e *Env) runBoot(b Boot, unpinned bool) (*Status, error) {
	o, err := e.l1Opts(b)
	if err != nil {
		return nil, err
	}
	out := e.outFor(b.Item)
	if e.DryRun {
		argv := L1Argv(e.Config.With(out, ""), o, "<payload>")
		fmt.Printf("  %s\n    suite=%s vendor=%s out=%s\n    %s\n", b.LogName, o.Suite, o.Vendor, out, strings.Join(argv, " "))
		return &Status{Boot: b, Outcome: &Outcome{}, Verdict: "dry"}, nil
	}
	// Pinning is for measurements: several L1s pinned to the same P-cores
	// would only fight over them.
	o.Pin = !unpinned
	o.Log = filepath.Join(e.Results, "logs", b.LogName+".log")
	start := time.Now()
	_, bootErr := BootL1(e.Config.With(out, ""), o)
	raw, _ := os.ReadFile(o.Log)
	oc := ParseLog(strings.NewReader(string(raw)))
	verdict, why := judge(b, oc, string(raw), 0, e.logChecks(b, o.Log))
	if bootErr != nil && verdict == "ok" {
		verdict, why = "FAIL", bootErr.Error()
	} else if bootErr != nil {
		why += " (" + bootErr.Error() + ")"
	}
	return &Status{Boot: b, Outcome: oc, Verdict: verdict, Why: why,
		Seconds: time.Since(start).Seconds()}, nil
}

// runPool runs boots, jobs at a time, printing and recording each as it
// finishes.  The statuses come back in the order of boots.
func (e *Env) runPool(boots []Boot, jobs int) ([]*Status, error) {
	if jobs < 1 {
		jobs = 1
	}
	statuses := make([]*Status, len(boots))
	errs := make([]error, len(boots))
	sem := make(chan struct{}, jobs)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, b := range boots {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, b Boot) {
			defer wg.Done()
			defer func() { <-sem }()
			s, err := e.runBoot(b, jobs > 1)
			mu.Lock()
			defer mu.Unlock()
			statuses[i], errs[i] = s, err
			if err != nil || e.DryRun {
				return
			}
			e.record(s)
			fmt.Printf("  %-4s %-48s %5.0fs  %s\n", s.Verdict, b.LogName, s.Seconds, s.Why)
		}(i, b)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return statuses, nil
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
	if dir == "" {
		return "unset"
	}
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
