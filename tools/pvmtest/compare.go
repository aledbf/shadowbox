package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// envID names a machine's baseline: the kernel it hosts on and its CPU model,
// the way scripts/perf-matrix.sh has always named them -- not the kernel
// under test, which is the thing being measured.
func envID() string {
	rel, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	model := ""
	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
				model = strings.TrimSpace(v)
				break
			}
		}
		f.Close()
	}
	model = regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(model, "-")
	model = strings.TrimRight(model, "-")
	if len(model) > 40 {
		model = model[:40]
	}
	return strings.TrimSpace(string(rel)) + "__" + model
}

type matrixKey struct {
	metric, vendor string
	cpus           int
}

type matrixRow struct {
	median, min, max float64
	n                int
	unit             string
}

func readMatrix(path string) (map[matrixKey]matrixRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows := map[matrixKey]matrixRow{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		c := strings.Split(sc.Text(), "\t")
		if len(c) < 8 || strings.HasPrefix(c[0], "#") || c[0] == "metric" {
			continue
		}
		cpus, _ := strconv.Atoi(c[1])
		md, _ := strconv.ParseFloat(c[3], 64)
		lo, _ := strconv.ParseFloat(c[4], 64)
		hi, _ := strconv.ParseFloat(c[5], 64)
		n, _ := strconv.Atoi(c[6])
		rows[matrixKey{c[0], c[2], cpus}] = matrixRow{md, lo, hi, n, c[7]}
	}
	return rows, sc.Err()
}

// CompareMatrix prints PVM against kvm-intel and against the baseline, and
// reports whether any PVM timing regressed: more than threshold percent worse
// *and* outside the range the baseline's own runs covered, since two sweeps
// of the same build have been seen 30% apart.
func CompareMatrix(w io.Writer, results, baseline string, threshold float64) (bool, error) {
	now, err := readMatrix(results)
	if err != nil {
		return false, err
	}
	var was map[matrixKey]matrixRow
	if baseline != "" {
		if was, err = readMatrix(baseline); err != nil {
			fmt.Fprintf(w, "no baseline at %s: make perf-baseline records this run as one\n", baseline)
			was = nil
		}
	}
	var keys []matrixKey
	for k, r := range now {
		if k.vendor == "pvm" && (r.unit == "ns" || r.unit == "us" || r.unit == "s") {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].metric != keys[j].metric {
			return keys[i].metric < keys[j].metric
		}
		return keys[i].cpus < keys[j].cpus
	})
	regressed := false
	fmt.Fprintf(w, "%-42s %5s %12s %7s %12s %8s %s\n", "metric", "cpus", "PVM", "spread", "KVM", "PVM/KVM", "vs baseline")
	for _, k := range keys {
		p := now[k]
		kvm, haveKVM := now[matrixKey{k.metric, "intel", k.cpus}]
		ratio, kvmS := "-", "-"
		if haveKVM && kvm.median > 0 {
			ratio = fmt.Sprintf("%.2fx", p.median/kvm.median)
			kvmS = fmt.Sprintf("%.1f", kvm.median)
		}
		spread := "-"
		if p.median > 0 {
			spread = fmt.Sprintf("%.0f%%", (p.max-p.min)/p.median*100)
		}
		delta := "-"
		if b, ok := was[k]; ok && b.median > 0 {
			d := (p.median - b.median) / b.median * 100
			delta = fmt.Sprintf("%+.1f%%", d)
			if d > threshold && p.median > b.max {
				delta += "  REGRESSION"
				regressed = true
			} else if d > threshold {
				delta += "  (inside baseline spread)"
			}
		}
		fmt.Fprintf(w, "%-42s %5d %12.1f %7s %12s %8s %s\n", k.metric, k.cpus, p.median, spread, kvmS, ratio, delta)
	}
	return regressed, nil
}
