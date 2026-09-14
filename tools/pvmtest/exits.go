package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ExitProfile says where a benchmark's exits go, attributed to the benchmark.
//
// The mmu mode counts events over the whole L1 run: boot, the case, teardown.
// For a case that runs for a second inside a three second run that is not an
// attribution -- PVM boots by emulating instructions one at a time until the
// guest reaches long mode, and that alone can outweigh whatever the case did.
// So run it twice per vendor, once with a case filter that matches nothing
// (boot and teardown, no case) and once with the case, and subtract.
func ExitProfile(c Config, testCase string, vendors []string) error {
	if len(vendors) == 0 {
		vendors = []string{"pvm", "intel"}
	}
	const noCase = "__baseline_no_case__"
	tmp, err := os.MkdirTemp("", "pvm-exits.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	base, cas := map[string]map[string]int64{}, map[string]map[string]int64{}
	var order []string
	for _, v := range vendors {
		for _, run := range []struct {
			only string
			into map[string]map[string]int64
		}{{noCase, base}, {testCase, cas}} {
			logf("%s: %s", v, map[bool]string{true: "baseline (boot and teardown, no case)", false: testCase}[run.only == noCase])
			log := filepath.Join(tmp, v+"-"+strings.ReplaceAll(run.only, "/", "_")+".log")
			if _, err := BootL1(c, L1Opts{Suite: "mmu", Vendor: v, Guest: []string{"pvmtest.only=" + run.only}, Log: log, Pin: true}); err != nil {
				warnf("%s: %v", v, err)
			}
			counts, names := exitCounts(log)
			run.into[v] = counts
			if run.only == testCase && len(order) == 0 {
				order = names
			}
			if run.only == testCase {
				if len(counts) == 0 {
					warnf("%s: no counters in the run -- did perf stat work?", v)
				}
				raw, _ := os.ReadFile(log)
				if !strings.Contains(string(raw), "ok 1 - "+testCase) {
					warnf("%s: '%s' did not report a pass; the delta is not the case", v, testCase)
				}
			}
		}
	}
	fmt.Printf("\n%-28s", "event")
	for _, v := range vendors {
		fmt.Printf("%14s", v)
	}
	if len(vendors) == 2 {
		fmt.Printf("%10s", "ratio")
	}
	fmt.Println()
	for _, e := range order {
		fmt.Printf("%-28s", e)
		var d []int64
		for _, v := range vendors {
			x := cas[v][e] - base[v][e]
			d = append(d, x)
			fmt.Printf("%14d", x)
		}
		if len(vendors) == 2 {
			// The first vendor over the second: "pvm intel" asks how much
			// worse PVM is, which is the question.
			if d[1] > 0 {
				fmt.Printf("%9.1fx", float64(d[0])/float64(d[1]))
			} else {
				fmt.Printf("%10s", "-")
			}
		}
		fmt.Println()
	}
	fmt.Println("\nDeltas: the case minus a run of the same guest with no case selected.\n" +
		"A negative number is noise -- boot and teardown are not identical between\n" +
		"two runs -- and says the case did not move that counter.")
	return nil
}

var (
	mmuLineRe = regexp.MustCompile(`^L1: mmu: *([0-9]+) *([a-z_]+:[a-z_]+)`)
	pvmStatRe = regexp.MustCompile(`PVMSTATS: vcpu=[0-9]+ (.*)$`)
)

// exitCounts reads the agent's "L1: mmu:" perf counts and, on a counting
// host, the PVMSTATS lines printed at teardown (summed over vCPUs, console
// copy only -- the agent echoes dmesg back under "L1: ").
func exitCounts(log string) (map[string]int64, []string) {
	counts := map[string]int64{}
	var order []string
	add := func(k string, v int64) {
		if _, ok := counts[k]; !ok {
			order = append(order, k)
		}
		counts[k] += v
	}
	f, err := os.Open(log)
	if err != nil {
		return counts, nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if m := mmuLineRe.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			add(m[2], n)
			continue
		}
		if strings.HasPrefix(line, "L1: ") {
			continue
		}
		if m := pvmStatRe.FindStringSubmatch(line); m != nil {
			for _, kv := range strings.Fields(m[1]) {
				k, v, ok := strings.Cut(kv, "=")
				if n, err := strconv.ParseInt(v, 10, 64); ok && err == nil {
					add("pvmstat:"+k, n)
				}
			}
		}
	}
	return counts, order
}

// ResolveExits turns the agent's "exitrip" lines into guest kernel symbols.
//
// The reason histogram says what the exits were; this says which guest code
// asked for them, which is the half you can act on.  The resolution happens
// here rather than in L1: the guest kernel is PIE and relocates itself at
// boot, so a RIP from the trace is a link address plus an offset only the
// running guest knows.  It prints that offset on every boot -- "PVMINIT: _text
// at ..." -- and vmlinux has the link address, so the difference is the delta.
func ResolveExits(c Config, log, vmlinux string) error {
	if vmlinux == "" {
		vmlinux = filepath.Join(c.Out, "build-guest", "vmlinux")
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		return err
	}
	m := regexp.MustCompile(`PVMINIT: _text at 0x([0-9a-f]+)`).FindSubmatch(raw)
	if m == nil {
		return fmt.Errorf("the log has no 'PVMINIT: _text at' line; was it a guest run?")
	}
	runtime, _ := strconv.ParseUint(string(m[1]), 16, 64)
	out, err := exec.Command("nm", "-n", vmlinux).Output()
	if err != nil {
		return fmt.Errorf("nm %s: %w", vmlinux, err)
	}
	type sym struct {
		addr uint64
		name string
	}
	var syms []sym
	var link uint64
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		a, err := strconv.ParseUint(f[0], 16, 64)
		if err != nil {
			continue
		}
		if f[2] == "_text" {
			link = a
		}
		if strings.ContainsAny(f[1], "tTwW") {
			syms = append(syms, sym{a, f[2]})
		}
	}
	if link == 0 {
		return fmt.Errorf("vmlinux has no _text symbol")
	}
	sort.Slice(syms, func(i, j int) bool { return syms[i].addr < syms[j].addr })
	delta := runtime - link
	logf("guest _text: runtime %#x, link %#x, delta %#x", runtime, link, delta)
	found := 0
	for _, l := range strings.Split(strings.ReplaceAll(string(raw), "\r", ""), "\n") {
		rest, ok := strings.CutPrefix(l, "L1: exitrip:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 3 {
			continue
		}
		// A reason can contain a space ("GP excp"): the count is first,
		// the rip last, the reason between.
		count, rip := f[0], f[len(f)-1]
		reason := strings.Join(f[1:len(f)-1], " ")
		r, err := strconv.ParseUint(strings.TrimPrefix(rip, "0x"), 16, 64)
		if err != nil {
			continue
		}
		want := r - delta
		i := sort.Search(len(syms), func(i int) bool { return syms[i].addr > want }) - 1
		if i >= 0 {
			fmt.Printf("%8s  %-22s %-18s %s+%#x\n", count, reason, rip, syms[i].name, want-syms[i].addr)
			found++
		} else {
			fmt.Printf("%8s  %-22s %-18s %s\n", count, reason, rip, "(not in vmlinux)")
		}
	}
	if found == 0 {
		return fmt.Errorf("no exitrip lines resolved -- is this an mmu-mode log?")
	}
	return nil
}

// Deps says what is missing and how to get it, once, rather than failing part
// way through a kernel build.
func Deps(c Config) error {
	missing := 0
	have := func(tool, hint string, required bool) {
		if p, err := exec.LookPath(tool); err == nil {
			fmt.Printf("  \033[32mok\033[0m   %-24s %s\n", tool, p)
			return
		}
		if required {
			fmt.Printf("  \033[31mmiss\033[0m %-24s %s\n", tool, hint)
			missing++
		} else {
			fmt.Printf("  \033[33mmiss\033[0m %-24s %s\n", tool, hint)
		}
	}
	fmt.Println("build:")
	for _, t := range [][2]string{{"make", "apt install build-essential"}, {"gcc", "apt install build-essential"},
		{"bison", "apt install bison"}, {"flex", "apt install flex"}, {"bc", "apt install bc"},
		{"go", "apt install golang-go, or from go.dev"}} {
		have(t[0], t[1], true)
	}
	have("pahole", "apt install dwarves  (optional: BTF)", false)
	fmt.Println("run:")
	have("qemu-system-x86_64", "apt install qemu-system-x86", true)
	have("mmdebstrap", "apt install mmdebstrap  (only needed for the L1 rootfs)", true)
	have("mkfs.ext4", "apt install e2fsprogs  (only needed for the L1 rootfs)", true)
	fmt.Println("profiling and debugging (optional):")
	haveHeader := func(h, pkg, what string) {
		cmd := exec.Command("gcc", "-E", "-")
		cmd.Stdin = strings.NewReader("#include <" + h + ">\n")
		if cmd.Run() == nil {
			fmt.Printf("  \033[32mok\033[0m   %-24s %s\n", pkg, what)
		} else {
			fmt.Printf("  \033[33mmiss\033[0m %-24s %s\n", pkg, what)
		}
	}
	haveHeader("traceevent/event-parse.h", "libtraceevent-dev", "REQUIRED for 'perf kvm stat' -- it is compiled out without this")
	haveHeader("elfutils/libdw.h", "libdw-dev", "DWARF: symbol resolution and --call-graph dwarf")
	haveHeader("libunwind.h", "libunwind-dev", "call graphs from a profile")
	haveHeader("capstone/capstone.h", "libcapstone-dev", "perf annotate: which instructions are hot")
	have("gdb", "attach to a guest kernel with qemu -s -S", false)
	have("fakechroot", "lets the rootfs build run without sudo", false)
	fmt.Println("kernel headers and libs:")
	for _, h := range []string{"libelf.h", "openssl/ssl.h"} {
		cmd := exec.Command("gcc", "-E", "-")
		cmd.Stdin = strings.NewReader("#include <" + h + ">\n")
		if cmd.Run() == nil {
			fmt.Printf("  \033[32mok\033[0m   %s\n", h)
		} else {
			fmt.Printf("  \033[31mmiss\033[0m %-16s apt install libelf-dev libssl-dev\n", h)
			missing++
		}
	}
	fmt.Println("host:")
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		f.Close()
		fmt.Println("  \033[32mok\033[0m   /dev/kvm writable")
	} else {
		fmt.Println("  \033[31mmiss\033[0m /dev/kvm  -- sudo usermod -aG kvm $USER, then log in again")
		missing++
	}
	// The PVM host refuses to load without these; better to find out now
	// than after building two kernels.
	cpuinfo, _ := os.ReadFile("/proc/cpuinfo")
	var flags string
	for _, l := range strings.Split(string(cpuinfo), "\n") {
		if strings.HasPrefix(l, "flags") {
			flags = " " + l + " "
			break
		}
	}
	for _, feat := range []string{"fsgsbase", "rdtscp", "cx16", "pcid", "invpcid"} {
		if strings.Contains(flags, " "+feat+" ") {
			fmt.Printf("  \033[32mok\033[0m   cpu has %s\n", feat)
		} else {
			fmt.Printf("  \033[31mmiss\033[0m cpu lacks %s -- kvm-pvm will refuse to load\n", feat)
			missing++
		}
	}
	if raw, err := os.ReadFile("/sys/devices/system/cpu/vulnerabilities/meltdown"); err == nil &&
		!strings.Contains(string(raw), "Not affected") {
		fmt.Printf("  \033[31mmiss\033[0m this CPU needs KPTI (%s) -- kvm-pvm will refuse to load\n", strings.TrimSpace(string(raw)))
		missing++
	}
	if missing > 0 {
		return fmt.Errorf("%d required item(s) missing", missing)
	}
	return nil
}
