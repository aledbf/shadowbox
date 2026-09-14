package main

import (
	"bufio"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// What one boot's log says.
type Outcome struct {
	Passed     []string
	Failed     []string // case names, from "not ok" lines and "# failed:"
	Results    []string // PVMTEST-RESULT / PVMHOSTTEST-RESULT words: ok|fail
	HostPass   int      // summed over host test programs
	HostFail   int
	Selftests  int    // "SELFTEST: <name> <verdict>" lines; CheckSelftests judges them
	KUT        int    // "KUT: <name> <verdict>" lines; CheckKUT judges them
	LoadFailed bool   // the vendor module did not load
	FailClosed string // suite failclosed: the agent's "FAILCLOSED: ok|fail" word
	Metrics    []Metric
	Complaints []string // kernel complaints outside the guest's own tests
	marks      map[int]string
	stats      map[int]map[string]int64
	statsKeys  []string
}

type Metric struct {
	Name  string
	Value float64
	Unit  string
}

var (
	// Guest lines come through L1 as "G[q35]: ", host test lines as "H: ".
	prefixRe  = regexp.MustCompile(`^(?:G\[[a-z0-9]+\]: |H: )`)
	tapRe     = regexp.MustCompile(`^(ok|not ok) \d+ - ([A-Za-z0-9_./+-]+)`)
	failedRe  = regexp.MustCompile(`^# failed: (.*)$`)
	resultRe  = regexp.MustCompile(`PVM(?:HOST)?TEST-RESULT: (ok|fail)(?: .*pass=(\d+) fail=(\d+))?`)
	hostResRe = regexp.MustCompile(`PVMHOSTTEST-RESULT: (?:ok|fail) pass=(\d+) fail=(\d+)`)
	metricRe  = regexp.MustCompile(`PVMTEST-METRIC: (\S+) (\S+) (\S+) #END`)
	markRe    = regexp.MustCompile(`PVMTEST-MARK: (\d+) (\S+) #END`)
	statsRe   = regexp.MustCompile(`PVMSTATS(-VM)?: mark=(\d+) (.*)$`)
	// What a kernel says when something is wrong with it.  The guest's own
	// negative tests make the guest print some of these on purpose, so
	// only the L1 host's lines count here; configs/log-fail.txt and
	// Sanitize (checks.go) remain the full scan.
	complaintRe = regexp.MustCompile(`(WARNING: CPU|BUG: |Oops|general protection fault|Kernel panic|RIP: 0010|soft lockup|rcu_sched self-detected|refcount_t: )`)
)

func ParseLog(r io.Reader) *Outcome {
	o := &Outcome{marks: map[int]string{}, stats: map[int]map[string]int64{}}
	seenKey := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	failed := map[string]bool{}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		fromL1Echo := strings.HasPrefix(line, "L1: ")
		body := prefixRe.ReplaceAllString(line, "")

		if m := tapRe.FindStringSubmatch(body); m != nil {
			if m[1] == "ok" {
				o.Passed = append(o.Passed, m[2])
			} else if !failed[m[2]] {
				failed[m[2]] = true
				o.Failed = append(o.Failed, m[2])
			}
			continue
		}
		if m := failedRe.FindStringSubmatch(body); m != nil {
			for _, name := range strings.Fields(m[1]) {
				name = strings.TrimSuffix(name, ",")
				if !failed[name] {
					failed[name] = true
					o.Failed = append(o.Failed, name)
				}
			}
			continue
		}
		if strings.HasPrefix(body, "SELFTEST: ") {
			o.Selftests++
			continue
		}
		if strings.HasPrefix(body, "KUT: ") {
			o.KUT++
			continue
		}
		if m := hostResRe.FindStringSubmatch(body); m != nil {
			p, _ := strconv.Atoi(m[1])
			f, _ := strconv.Atoi(m[2])
			o.HostPass += p
			o.HostFail += f
		}
		if m := resultRe.FindStringSubmatch(body); m != nil && !fromL1Echo {
			o.Results = append(o.Results, m[1])
			continue
		}
		if m := metricRe.FindStringSubmatch(body); m != nil {
			v, err := strconv.ParseFloat(m[2], 64)
			if err == nil {
				o.Metrics = append(o.Metrics, Metric{m[1], v, m[3]})
			}
			continue
		}
		if strings.HasPrefix(line, "FAILCLOSED: ") {
			o.FailClosed = strings.Fields(line)[1]
		}
		if strings.Contains(body, "L1: insmod failed") || strings.HasPrefix(line, "L1: insmod failed") {
			o.LoadFailed = true
		}
		if fromL1Echo {
			continue
		}
		if m := markRe.FindStringSubmatch(body); m != nil {
			v, _ := strconv.Atoi(m[1])
			if v%2 == 0 {
				name := strings.TrimSuffix(strings.TrimSuffix(m[2], "/before"), "/after")
				o.marks[v] = name
			}
			continue
		}
		if m := statsRe.FindStringSubmatch(body); m != nil {
			mark, _ := strconv.Atoi(m[2])
			vm := m[1] != ""
			if o.stats[mark] == nil {
				o.stats[mark] = map[string]int64{}
			}
			for _, kv := range strings.Fields(m[3]) {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "vcpu" {
					continue
				}
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					continue
				}
				if vm {
					k = "vm." + k
				}
				if !seenKey[k] {
					seenKey[k] = true
					o.statsKeys = append(o.statsKeys, k)
				}
				o.stats[mark][k] += n
			}
			continue
		}
		if !strings.HasPrefix(line, "G[") && complaintRe.MatchString(line) {
			o.Complaints = append(o.Complaints, line)
		}
	}
	return o
}

// Unexpected is every failed case not allowed to fail.  A name the console
// garbled -- L1's printk landing in the middle of a guest line -- is taken to
// be an allowed one if an allowed name starts with it.
func (o *Outcome) Unexpected(allow []string) []string {
	var out []string
	for _, f := range o.Failed {
		ok := false
		for _, a := range allow {
			if f == a || (len(f) >= 8 && strings.HasPrefix(a, f)) {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, f)
		}
	}
	return out
}

// StatsPair is one bracketed phase: counters summed over vCPUs, after minus
// before.
type StatsPair struct {
	Name   string
	Deltas map[string]int64
}

func (o *Outcome) StatsPairs() ([]StatsPair, []string) {
	var evens []int
	for v := range o.marks {
		if o.stats[v] != nil && o.stats[v+1] != nil {
			evens = append(evens, v)
		}
	}
	sort.Ints(evens)
	var pairs []StatsPair
	for _, v := range evens {
		d := map[string]int64{}
		for _, k := range o.statsKeys {
			d[k] = o.stats[v+1][k] - o.stats[v][k]
		}
		pairs = append(pairs, StatsPair{Name: o.marks[v], Deltas: d})
	}
	return pairs, o.statsKeys
}
