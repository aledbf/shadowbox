package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
)

type sample struct {
	metric, vendor, side, unit string
	cpus                       int
	value                      float64
}

func collect(statuses []*Status) []sample {
	var out []sample
	for _, s := range statuses {
		if s.Verdict != "ok" {
			continue
		}
		for _, m := range s.Outcome.Metrics {
			out = append(out, sample{m.Name, s.Boot.Vendor, s.Boot.Side, m.Unit, s.Boot.CPUs, m.Value})
		}
	}
	return out
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return math.NaN()
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func minmax(v []float64) (float64, float64) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, x := range v {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	return lo, hi
}

// WriteMatrix writes medians in the baselines/*.tsv format
// (the format the old perf scripts wrote), so a matrix from here can be
// a baseline and be compared against one.
func WriteMatrix(w io.Writer, name, kernel string, reps int, statuses []*Status) {
	type key struct {
		metric, vendor string
		cpus           int
	}
	vals := map[key][]float64{}
	units := map[key]string{}
	for _, s := range collect(statuses) {
		k := key{s.metric, s.vendor, s.cpus}
		vals[k] = append(vals[k], s.value)
		units[k] = s.unit
	}
	keys := make([]key, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.metric != b.metric {
			return a.metric < b.metric
		}
		if a.cpus != b.cpus {
			return a.cpus < b.cpus
		}
		return a.vendor < b.vendor
	})
	fmt.Fprintf(w, "# pvm-testbed perf matrix\n# env\t%s\n# kernel\t%s\n# reps\t%d\n", name, kernel, reps)
	fmt.Fprintln(w, "metric\tcpus\tvendor\tmedian\tmin\tmax\tn\tunit")
	for _, k := range keys {
		lo, hi := minmax(vals[k])
		fmt.Fprintf(w, "%s\t%d\t%s\t%.6g\t%.6g\t%.6g\t%d\t%s\n", k.metric, k.cpus, k.vendor,
			median(vals[k]), lo, hi, len(vals[k]), units[k])
	}
}

// WriteAB: per metric and vCPU count, the medians of both sides, the delta,
// and whether the two sides' ranges are disjoint -- the plainest statement
// that a difference is outside the noise of five runs.
func WriteAB(w io.Writer, statuses []*Status) {
	type key struct {
		metric string
		cpus   int
	}
	a, b := map[key][]float64{}, map[key][]float64{}
	for _, s := range collect(statuses) {
		k := key{s.metric, s.cpus}
		if s.side == "a" {
			a[k] = append(a[k], s.value)
		} else {
			b[k] = append(b[k], s.value)
		}
	}
	keys := make([]key, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].metric != keys[j].metric {
			return keys[i].metric < keys[j].metric
		}
		return keys[i].cpus < keys[j].cpus
	})
	fmt.Fprintln(w, "metric\tcpus\tA\tB\tdelta\tB/A\tnA\tnB\tA_range\tB_range\tdisjoint")
	for _, k := range keys {
		if len(b[k]) == 0 {
			continue
		}
		ma, mb := median(a[k]), median(b[k])
		alo, ahi := minmax(a[k])
		blo, bhi := minmax(b[k])
		disjoint := "no"
		if ahi < blo || bhi < alo {
			disjoint = "yes"
		}
		fmt.Fprintf(w, "%s\t%d\t%.6g\t%.6g\t%+.1f%%\t%.2fx\t%d\t%d\t%.6g-%.6g\t%.6g-%.6g\t%s\n",
			k.metric, k.cpus, ma, mb, 100*(mb-ma)/ma, mb/ma, len(a[k]), len(b[k]),
			alo, ahi, blo, bhi, disjoint)
	}
}

// printTSV aligns a tab-separated table for the terminal.
func printTSV(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var rows [][]string
	width := map[int]int{}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		for i, c := range cols {
			if len(c) > width[i] {
				width[i] = len(c)
			}
		}
		rows = append(rows, cols)
	}
	for _, r := range rows {
		for i, c := range r {
			fmt.Printf("%-*s  ", width[i], c)
		}
		fmt.Println()
	}
}
