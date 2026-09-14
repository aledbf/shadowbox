// pvmtest runs pvm-testbed batteries: every boot the testbed does, described
// in a file under batteries/ instead of a shell loop written for the occasion.
//
//	pvmtest run   [-n] [-only item,...] <battery>   run it; exit 1 on any unexpected result
//	pvmtest list  <battery>                         the boots it would do
//	pvmtest stats [-filter re] <log>                per-case counter deltas of a log
//	pvmtest check <log> [allow-fail,...]            judge one log as a run item would
//	pvmtest compare <matrix.tsv> <baseline.tsv>     PVM vs KVM vs baseline, exit 1 on regression
//
// A run writes out/results/<battery>-<time>/: manifest.txt (battery, kernel
// and testbed revisions, host build), summary.tsv (one row per boot, with
// the verdict), metrics.tsv, stats-<boot>.tsv for boots with stats=on,
// matrix-<item>.tsv and ab-<item>.tsv for those items, and logs/.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		os.Exit(cmdRun(os.Args[2:], false))
	case "list":
		os.Exit(cmdRun(os.Args[2:], true))
	case "stats":
		os.Exit(cmdStats(os.Args[2:]))
	case "check":
		os.Exit(cmdCheck(os.Args[2:]))
	case "compare":
		if len(os.Args) != 4 {
			usage()
		}
		regressed, err := CompareMatrix(os.Stdout, os.Args[2], os.Args[3], 15)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if regressed {
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  pvmtest run   [-n] [-only item,...] <battery>
  pvmtest list  <battery>
  pvmtest stats [-filter regexp] <log>
  pvmtest check <log> [allow-fail,...]
  pvmtest compare <matrix.tsv> <baseline.tsv>`)
	os.Exit(2)
}

func testbedDir() string {
	if d := os.Getenv("TESTBED"); d != "" {
		return d
	}
	exe, err := os.Executable()
	if err == nil {
		// out/bin/pvmtest -> the testbed root.
		d := filepath.Dir(filepath.Dir(filepath.Dir(exe)))
		if _, err := os.Stat(filepath.Join(d, "scripts", "run-l1.sh")); err == nil {
			return d
		}
	}
	wd, _ := os.Getwd()
	return wd
}

func cmdRun(args []string, listOnly bool) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dry := fs.Bool("n", false, "print the boots and their commands, run nothing")
	only := fs.String("only", "", "comma separated item names to run")
	jobs := fs.Int("j", 1, "boots of run items at once (never for matrix/ab); unpinned when > 1")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	bat, err := ParseBattery(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	tb := testbedDir()
	env := &Env{Testbed: tb, Out: filepath.Join(tb, "out"), DryRun: *dry || listOnly}
	ksrc := os.Getenv("KSRC")
	if ksrc == "" {
		ksrc = "/home/aledbf/Trabajo/github/linux-aledbf"
	}

	var items []Item
	want := map[string]bool{}
	for _, n := range strings.Split(*only, ",") {
		if n != "" {
			want[n] = true
		}
	}
	for _, it := range bat.Items {
		if len(want) == 0 || want[it.Name] {
			items = append(items, it)
		}
	}

	host := env.hostVariant()
	for _, it := range items {
		if need := it.Get("host", ""); need != "" && need != host && !env.DryRun {
			fmt.Fprintf(os.Stderr, "%s:%d: %s needs a %s host build, out/build-host is %s:\n"+
				"  scripts/build-kernel.sh host%s\n", bat.Path, it.Line, it.Name, need, host,
				map[string]string{"stats": " with HOST_CONFIG_EXTRA=CONFIG_KVM_PVM_STATS=y", "timing": ""}[need])
			return 2
		}
		if it.Get("stats", "") == "on" && host != "stats" && !env.DryRun {
			fmt.Fprintf(os.Stderr, "%s:%d: %s has stats=on but the host is a %s build\n",
				bat.Path, it.Line, it.Name, host)
			return 2
		}
	}

	name := strings.TrimSuffix(filepath.Base(bat.Path), filepath.Ext(bat.Path))
	if !env.DryRun {
		env.Results = filepath.Join(env.Out, "results", name+"-"+time.Now().Format("20060102-150405"))
		if err := os.MkdirAll(filepath.Join(env.Results, "logs"), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		manifest := fmt.Sprintf("battery\t%s\nkernel\t%s\ntestbed\t%s\nhost-build\t%s\nstarted\t%s\n\n%s",
			bat.Path, gitDescribe(ksrc), gitDescribe(tb), host, time.Now().Format(time.RFC3339), bat.Text)
		writeFile(filepath.Join(env.Results, "manifest.txt"), manifest)
		env.summary, _ = os.Create(filepath.Join(env.Results, "summary.tsv"))
		env.metrics, _ = os.Create(filepath.Join(env.Results, "metrics.tsv"))
		fmt.Fprintln(env.summary, "boot\titem\tvendor\tcpus\trep\tside\tverdict\tseconds\twhy")
		fmt.Fprintln(env.metrics, "boot\titem\tvendor\tcpus\trep\tside\tmetric\tvalue\tunit")
		fmt.Printf("results: %s\n", env.Results)
	}

	bad := 0
	// Consecutive run items share a pool of *jobs boots; matrix and ab
	// items measure, so they run alone and in order.
	var pending []Boot
	flush := func() int {
		if len(pending) == 0 {
			return 0
		}
		statuses, err := env.runPool(pending, *jobs)
		pending = nil
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return -1
		}
		n := 0
		for _, s := range statuses {
			if s.Verdict != "ok" && s.Verdict != "dry" {
				n++
			}
		}
		return n
	}
	for _, it := range items {
		boots, err := Boots(it)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s:%d: %v\n", bat.Path, it.Line, err)
			return 2
		}
		fmt.Printf("== %s (%d boot%s)\n", it.String(), len(boots), map[bool]string{true: "", false: "s"}[len(boots) == 1])
		if it.Kind == "run" {
			pending = append(pending, boots...)
			continue
		}
		if n := flush(); n < 0 {
			return 2
		} else {
			bad += n
		}
		statuses, err := env.runPool(boots, 1)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if env.DryRun {
			continue
		}
		switch it.Kind {
		case "matrix":
			path := filepath.Join(env.Results, "matrix-"+it.Name+".tsv")
			f, _ := os.Create(path)
			WriteMatrix(f, envID(), gitDescribe(ksrc), atoiDef(it.Get("reps", "1"), 1), statuses)
			f.Close()
			// Where make perf-baseline looks for the latest sweep.
			os.MkdirAll(filepath.Join(env.Out, "perf"), 0o755)
			if raw, err := os.ReadFile(path); err == nil {
				writeFile(filepath.Join(env.Out, "perf", envID()+".tsv"), string(raw))
			}
			baseline := it.Get("baseline", "auto")
			switch baseline {
			case "auto":
				baseline = filepath.Join(tb, "baselines", envID()+".tsv")
			case "none":
				baseline = ""
			}
			threshold, _ := strconv.ParseFloat(it.Get("threshold", "15"), 64)
			regressed, err := CompareMatrix(os.Stdout, path, baseline, threshold)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			if regressed {
				fmt.Printf("%s: at least one PVM timing is more than %g%% worse than the baseline\n", it.Name, threshold)
				bad++
			}
		case "ab":
			path := filepath.Join(env.Results, "ab-"+it.Name+".tsv")
			f, _ := os.Create(path)
			WriteAB(f, statuses)
			f.Close()
			printTSV(path)
		}
		for _, s := range statuses {
			if s.Verdict != "ok" {
				fmt.Printf("  (dropped from %s: %s: %s)\n", it.Name, s.Boot.LogName, s.Why)
			}
		}
	}
	if n := flush(); n < 0 {
		return 2
	} else {
		bad += n
	}
	if !env.DryRun {
		env.summary.Close()
		env.metrics.Close()
		fmt.Printf("results: %s\n", env.Results)
		if bad > 0 {
			fmt.Printf("%d unexpected result(s)\n", bad)
			return 1
		}
	}
	return 0
}

func cmdStats(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	filter := fs.String("filter", ".", "counter name regexp")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer f.Close()
	re, err := regexp.Compile(*filter)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	pairs, keys := ParseLog(f).StatsPairs()
	if len(pairs) == 0 {
		fmt.Fprintln(os.Stderr, "no complete mark pairs: stats=on on a stats host?")
		return 1
	}
	var sel []string
	for _, k := range keys {
		if re.MatchString(k) {
			sel = append(sel, k)
		}
	}
	tmp, _ := os.CreateTemp("", "pvmtest-stats")
	writeStats(tmp.Name(), pairs, sel)
	tmp.Close()
	printTSV(tmp.Name())
	os.Remove(tmp.Name())
	return 0
}

func cmdCheck(args []string) int {
	if len(args) < 1 {
		usage()
	}
	f, err := os.Open(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer f.Close()
	it := Item{Kind: "run", Name: filepath.Base(args[0]), Keys: map[string]string{}}
	if len(args) > 1 {
		it.Keys["allow-fail"] = args[1]
	}
	raw, _ := os.ReadFile(args[0])
	verdict, why := judge(Boot{Item: it}, ParseLog(strings.NewReader(string(raw))), string(raw), 0, nil)
	fmt.Printf("%s: %s\n", verdict, why)
	if verdict != "ok" {
		return 1
	}
	return 0
}
