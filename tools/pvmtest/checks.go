package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Sanitize scans boot and run logs for the things a kernel says when it has
// found a problem with itself, and fails if any of them are not on the
// allowlist.
//
// This is the cheapest bug detector in the testbed: it needs nobody to have
// written a test for the thing that went wrong.  A WARN_ON that fires once in
// a fork storm, an RCU stall under the shadow MMU's mmu_lock, a refcount
// underflow on a PVCS page -- none of those make a test case fail, and all of
// them show up here.
//
// Two data files drive it, both greppable and both commented:
//
//	configs/log-fail.txt   what counts as the kernel complaining
//	configs/log-allow.txt  which of those complaints are known and why
//
// Nesting: an L1 log carries the L1 kernel's own output, the guest's output
// behind a "G[machine]:" prefix, and L1's copy of ftrace behind "L1: ".  The
// prefixes are replaced by a layer tag before matching -- G the PVM guest, T
// L1's ftrace relay, "." the log's own kernel -- so a stall inside the guest
// is caught with the same patterns as one in the host, and the report says
// which layer it came from.  The patterns see the tagged line, as they
// always have.
func Sanitize(c Config, files []string, quiet bool) error {
	fail, err := loadPatterns(filepath.Join(c.Testbed, "configs", "log-fail.txt"))
	if err != nil {
		return err
	}
	allow, err := loadPatterns(filepath.Join(c.Testbed, "configs", "log-allow.txt"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		files, _ = filepath.Glob(filepath.Join(c.Out, "logs", "*.log"))
	}
	if len(files) == 0 {
		return fmt.Errorf("no logs to scan -- run something first")
	}
	guestRe := regexp.MustCompile(`^G\[[a-z0-9_.-]*\]: `)
	totalHit, totalKnown, scanned := 0, 0, 0
	for _, f := range files {
		// qemu's own -d output is machine trace, not kernel log.
		if strings.Contains(filepath.Base(f), "qemu-debug") {
			continue
		}
		fh, err := os.Open(f)
		if err != nil {
			warnf("no such log: %s", f)
			continue
		}
		scanned++
		var unknown []string
		known := 0
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case guestRe.MatchString(line):
				line = "G\t" + guestRe.ReplaceAllString(line, "")
			case strings.HasPrefix(line, "L1: "):
				line = "T\t" + strings.TrimPrefix(line, "L1: ")
			default:
				line = ".\t" + line
			}
			if !matchAny(fail, line) {
				continue
			}
			if matchAny(allow, line) {
				known++
			} else {
				unknown = append(unknown, line)
			}
		}
		fh.Close()
		totalKnown += known
		if len(unknown) == 0 {
			if !quiet && known > 0 {
				logf("%s: %d known, 0 unknown", filepath.Base(f), known)
			}
			continue
		}
		totalHit += len(unknown)
		fmt.Fprintf(os.Stderr, "\n\033[1;31m%s\033[0m  %d unknown, %d known\n", filepath.Base(f), len(unknown), known)
		// Collapse everything that differs run to run -- the printk
		// timestamp, addresses, counters, CPU numbers -- so a stall
		// reported five times reads as one finding rather than five.
		for _, g := range collapse(unknown) {
			fmt.Fprintf(os.Stderr, "  %3dx  %s\n", g.n, g.line)
		}
	}
	if totalHit > 0 {
		fmt.Fprintf(os.Stderr, "\n\033[1;31msanitize-log: %d unexplained kernel complaints in %d logs\033[0m\n", totalHit, scanned)
		fmt.Fprintf(os.Stderr, "If one of them is understood and expected, add it to\n  configs/log-allow.txt  -- with the reason.\n")
		return fmt.Errorf("%d unexplained kernel complaints", totalHit)
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "\033[32msanitize-log: %d logs clean (%d known lines allowed)\033[0m\n", scanned, totalKnown)
	}
	return nil
}

func loadPatterns(path string) ([]*regexp.Regexp, error) {
	lines, err := readList(path)
	if err != nil {
		return nil, err
	}
	var out []*regexp.Regexp
	for _, l := range lines {
		re, err := regexp.Compile(l)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %w", path, l, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

type group struct {
	n    int
	line string
}

var (
	tsRe    = regexp.MustCompile(`\[[0-9 ]+\.[0-9]+\] `)
	hex0xRe = regexp.MustCompile(`0x[0-9a-f]+`)
	hexRe   = regexp.MustCompile(`\b[0-9a-f]{8,}\b`)
	numRe   = regexp.MustCompile(`[0-9]+`)
	spaceRe = regexp.MustCompile(`\s+`)
)

func collapse(lines []string) []group {
	count := map[string]int{}
	for _, l := range lines {
		l = tsRe.ReplaceAllString(l, "")
		l = hex0xRe.ReplaceAllString(l, "0xX")
		l = hexRe.ReplaceAllString(l, "HEX")
		l = numRe.ReplaceAllString(l, "N")
		l = spaceRe.ReplaceAllString(l, " ")
		count[l]++
	}
	var gs []group
	for l, n := range count {
		gs = append(gs, group{n, l})
	}
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].n != gs[j].n {
			return gs[i].n > gs[j].n
		}
		return gs[i].line < gs[j].line
	})
	return gs
}

// CheckSelftests compares the SELFTEST: lines an L1 run produced against what
// configs/kvm-selftests-expect.txt says that vendor should do, and fails on
// any difference -- in either direction.
//
// Something that starts failing is a regression.  Something that starts
// passing is a gap that closed, and the expectation should be updated
// deliberately rather than left saying the opposite.  Both are news, so both
// are reported.  A test that is listed but never ran is as much of a problem:
// it means the build or the payload dropped it.
func CheckSelftests(c Config, logPath, vendor string) error {
	return checkExpected(filepath.Join(c.Testbed, "configs", "kvm-selftests-expect.txt"), logPath, "SELFTEST: ", vendor)
}

// checkExpected holds the "<prefix><name> <verdict>" lines of a log to the
// rows of an expectation file whose second column is key.
func checkExpected(expectPath, logPath, prefix, key string) error {
	expectName := filepath.Base(expectPath)
	expect, err := readList(expectPath)
	if err != nil {
		return err
	}
	want := map[string]string{}
	var order []string
	for _, l := range expect {
		f := strings.Fields(l)
		if len(f) >= 3 && f[1] == key {
			want[f[0]] = f[2]
			order = append(order, f[0])
		}
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	fails := 0
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r", ""), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		f := strings.Fields(strings.TrimPrefix(line, prefix))
		if len(f) < 2 {
			continue
		}
		name, verdict := f[0], f[1]
		seen[name] = true
		w, ok := want[name]
		switch {
		case !ok:
			warnf("%s: no expectation recorded for %s (got %s)", name, key, verdict)
			fails++
		case verdict != w && (w == "fail" || w == "skip"):
			fmt.Fprintf(os.Stderr, "\033[1;33mCHANGED\033[0m %-34s %s -> %s  (a gap may have closed; update configs/%s)\n", name, w, verdict, expectName)
			fails++
		case verdict != w:
			fmt.Fprintf(os.Stderr, "\033[1;31mREGRESSED\033[0m %-32s %s -> %s\n", name, w, verdict)
			fails++
		}
	}
	if len(seen) == 0 {
		return fmt.Errorf("the log has no %s lines at all", strings.TrimSpace(prefix))
	}
	for _, name := range order {
		if !seen[name] {
			warnf("%s is expected for %s but did not run", name, key)
			fails++
		}
	}
	if fails > 0 {
		return fmt.Errorf("results differ from configs/%s", expectName)
	}
	logf("%s (%s): %d tests, all as recorded", strings.TrimSuffix(prefix, ": "), key, len(seen))
	return nil
}
