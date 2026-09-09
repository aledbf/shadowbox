// Package harness runs the test cases and prints the result in a form
// the outside script can match on.
//
// Output is TAP, because it is line oriented and survives being
// interleaved with kernel printk on the same serial console, which is
// exactly what happens here.  The last line is a single PVMTEST-RESULT,
// and the script outside keys off that and nothing else.
package harness

import (
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

// Suite membership.  A case can be in several.
const (
	Smoke = "smoke"   // did the kernel boot at all
	Core  = "default" // the everyday run
	Full  = "full"    // everything, including the slow and the invasive
	Perf  = "perf"    // measurements, not assertions

	// Security is the negative tests: things the guest must not be able
	// to do.  Like Perf it is outside the smoke/default/full ranking, so
	// "default" does not drag it in; cases list Full alongside it when
	// they should also run there.
	Security = "security"
)

type Case struct {
	Name    string
	Suites  []string
	Timeout time.Duration
	Fn      func(t *T) error
}

// T is what a case gets.  Deliberately thin: a case either returns an
// error or it does not.  Logf output is indented under the case in the
// TAP stream so a failure is readable without the source next to it.
type T struct {
	name  string
	notes []string
}

func (t *T) Logf(format string, a ...any) {
	t.notes = append(t.notes, fmt.Sprintf(format, a...))
}

// Metric records a number for the perf suite.  It is printed rather than
// asserted on: thresholds belong outside, where several runs can be
// compared, not baked into the guest.
func (t *T) Metric(name string, value float64, unit string) {
	t.notes = append(t.notes, fmt.Sprintf("metric %s = %g %s", name, value, unit))
	fmt.Printf("PVMTEST-METRIC: %s.%s %g %s\n", t.name, name, value, unit)
}

type Harness struct {
	suite string
	tag   string
	only  string // substring filter, for profiling one case at a time
	cases []Case

	pass, fail, skip int
	failed           []string
}

func New(suite, tag string) *Harness {
	return &Harness{suite: suite, tag: tag}
}

// Only narrows the run to cases whose name contains sub.  A profile of
// four benchmarks says less than a profile of the one being asked about.
func (h *Harness) Only(sub string) { h.only = sub }

func (h *Harness) Add(c Case) {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	h.cases = append(h.cases, c)
}

func (h *Harness) selected(c Case) bool {
	if h.only != "" && !strings.Contains(c.Name, h.only) {
		return false
	}
	if h.suite == "all" {
		return true
	}
	for _, s := range c.Suites {
		if s == h.suite {
			return true
		}
	}
	// "full" is a superset of "default", which is a superset of "smoke".
	rank := map[string]int{Smoke: 0, Core: 1, Full: 2, Perf: 3}
	want, ok := rank[h.suite]
	if !ok {
		return false
	}
	if want == rank[Perf] {
		return false
	}
	for _, s := range c.Suites {
		if r, ok := rank[s]; ok && r <= want && r != rank[Perf] {
			return true
		}
	}
	return false
}

func (h *Harness) Run() {
	sort.SliceStable(h.cases, func(i, j int) bool { return h.cases[i].Name < h.cases[j].Name })

	var run []Case
	for _, c := range h.cases {
		if h.selected(c) {
			run = append(run, c)
		} else {
			h.skip++
		}
	}

	fmt.Printf("1..%d\n", len(run))
	for i, c := range run {
		h.runOne(i+1, c)
	}
}

func (h *Harness) runOne(n int, c Case) {
	t := &T{name: c.Name}

	// A case that hangs must not take the whole run down with it: the
	// outside timeout would kill the VM before any result was printed,
	// and "timed out" without a name is the least useful failure there
	// is.  The goroutine is left running -- there is no safe way to stop
	// it -- but the run continues and the log names the culprit.
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v\n%s", r, debug.Stack())
			}
		}()
		done <- c.Fn(t)
	}()

	var err error
	start := time.Now()
	select {
	case err = <-done:
	case <-time.After(c.Timeout):
		err = fmt.Errorf("timed out after %s", c.Timeout)
	}
	dur := time.Since(start)

	if err != nil {
		h.fail++
		h.failed = append(h.failed, c.Name)
		fmt.Printf("not ok %d - %s (%s)\n", n, c.Name, dur.Round(time.Millisecond))
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Printf("#   %s\n", line)
		}
	} else {
		h.pass++
		fmt.Printf("ok %d - %s (%s)\n", n, c.Name, dur.Round(time.Millisecond))
	}
	for _, note := range t.notes {
		fmt.Printf("#   %s\n", note)
	}
	os.Stdout.Sync()
}

// Report prints the one line the outside harness greps for.  It has to be
// last, and it has to be printed even when everything went wrong.
func (h *Harness) Report(elapsed time.Duration) {
	status := "ok"
	if h.fail > 0 || h.pass == 0 {
		status = "fail"
	}
	fmt.Printf("\n# ran %d, passed %d, failed %d, not selected %d, in %s\n",
		h.pass+h.fail, h.pass, h.fail, h.skip, elapsed.Round(time.Millisecond))
	if len(h.failed) > 0 {
		fmt.Printf("# failed: %s\n", strings.Join(h.failed, " "))
	}
	fmt.Printf("PVMTEST-RESULT: %s tag=%s suite=%s pass=%d fail=%d\n",
		status, h.tag, h.suite, h.pass, h.fail)
	os.Stdout.Sync()
}
