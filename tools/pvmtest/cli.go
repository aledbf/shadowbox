package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	*l = append(*l, strings.Split(v, ",")...)
	return nil
}

// cmdBuild: pvmtest build guest|host|initrd|rootfs|hosttests|agent|selftests|kut|ref|all
func cmdBuild(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("pvmtest build guest|host [-stats] [-extra CONFIG_X=y]|initrd|rootfs|hosttests|agent|selftests|kut|ref <name> <git> <rev> [timing|stats]|all")
	}
	c := LoadConfig()
	what := args[0]
	fs := flag.NewFlagSet("build "+what, flag.ExitOnError)
	stats := fs.Bool("stats", false, "host: CONFIG_KVM_PVM_STATS=y, a counting build (never for timings)")
	var extra listFlag
	fs.Var(&extra, "extra", "CONFIG_FOO=y|m|n on top of the fragments (repeatable, or comma separated)")
	fs.Parse(args[1:])
	if *stats {
		extra = append(extra, "CONFIG_KVM_PVM_STATS=y")
	}
	switch what {
	case "guest", "host":
		return BuildKernel(c, what, extra)
	case "initrd":
		return BuildInitrd(c)
	case "rootfs":
		return BuildRootfs(c)
	case "hosttests":
		return BuildHosttests(c)
	case "agent":
		return BuildAgent(c)
	case "selftests":
		return BuildSelftests(c)
	case "kut":
		return BuildKUT(c)
	case "ref":
		a := fs.Args()
		if len(a) < 3 {
			return fmt.Errorf("pvmtest build ref <name> <git-dir> <rev> [timing|stats]")
		}
		variant := "timing"
		if len(a) > 3 {
			variant = a[3]
		}
		return BuildRef(c, a[0], a[1], a[2], variant)
	case "all":
		for _, step := range []func() error{
			func() error { return BuildKernel(c, "guest", nil) },
			func() error { return BuildKernel(c, "host", extra) },
			func() error { return BuildInitrd(c) },
			func() error { return BuildAgent(c) },
			func() error { return BuildHosttests(c) },
		} {
			if err := step(); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("build: unknown target %q", what)
}

// cmdBoot: pvmtest boot l1|guest|host-selftest [flags]
func cmdBoot(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("pvmtest boot l1|guest|host-selftest [flags]")
	}
	c := LoadConfig()
	switch args[0] {
	case "l1":
		fs := flag.NewFlagSet("boot l1", flag.ExitOnError)
		o := L1Opts{Echo: true, Pin: true}
		var guest, mod, l1args listFlag
		fs.StringVar(&o.Suite, "suite", "default", "guest suite, or an agent mode: profile, lock, mmu, hosttests, failclosed")
		fs.StringVar(&o.Vendor, "vendor", "pvm", "pvm or intel")
		fs.StringVar(&o.CPUs, "cpus", os.Getenv("GUEST_CPUS"), "guest vCPUs")
		fs.StringVar(&o.Mem, "mem", os.Getenv("GUEST_MEM"), "guest memory")
		fs.Var(&guest, "guest", "guest kernel/harness arguments, e.g. pvmtest.only=perf/syscall")
		fs.Var(&mod, "mod", "vendor module arguments")
		fs.Var(&l1args, "l1append", "extra L1 kernel arguments")
		fs.StringVar(&o.Profile, "profile", "", "case to profile (suite profile)")
		l1 := fs.String("l1", "kvm", "kvm, tcg or tcg-la57")
		pti := fs.Bool("pti", false, "boot L1 with pti=on (kvm-pvm will refuse)")
		timeout := fs.Int("timeout", 0, "seconds (default depends on the suite)")
		fs.StringVar(&o.Log, "log", "", "log path (default out/logs/l1-<suite>-<vendor>.log)")
		kernel := fs.String("kernel", "", "image set out/refs/<name> instead of out/")
		nopin := fs.Bool("nopin", false, "do not pin to the fast cores")
		fs.Parse(args[1:])
		o.Guest, o.Mod, o.L1Args = guest, mod, l1args
		if *pti {
			o.L1Args = append(o.L1Args, "pti=on")
		}
		switch *l1 {
		case "kvm":
		case "tcg", "tcg-la57":
			o.Accel, o.CPU, o.Timeout = "tcg", "max", 20000*time.Second
			if *l1 == "tcg-la57" {
				o.CPU = "max,la57=on"
			}
			o.L1Args = append(o.L1Args, "pvmtest.guest_timeout=15000")
		default:
			return fmt.Errorf("-l1: kvm, tcg or tcg-la57")
		}
		if *timeout > 0 {
			o.Timeout = time.Duration(*timeout) * time.Second
		}
		o.Pin = !*nopin
		o.QMP, o.Debug = os.Getenv("L1_QMP"), os.Getenv("L1_QEMU_DEBUG")
		if *kernel != "" {
			c = c.With(c.Out+"/refs/"+*kernel, "")
		}
		log, err := BootL1(c, o)
		if err != nil {
			return err
		}
		raw, _ := os.ReadFile(log)
		it := Item{Kind: "run", Name: "boot", Keys: map[string]string{"suite": o.Suite}}
		verdict, why := judge(Boot{Item: it, Vendor: o.Vendor}, ParseLog(strings.NewReader(string(raw))), string(raw), 0,
			(&Env{Config: c}).logChecks(Boot{Item: it, Vendor: o.Vendor}, log))
		logf("%s: %s (%s)", verdict, why, log)
		if verdict != "ok" {
			return fmt.Errorf("%s", why)
		}
		return nil
	case "guest":
		fs := flag.NewFlagSet("boot guest", flag.ExitOnError)
		o := GuestOpts{Echo: true}
		fs.StringVar(&o.Boot, "boot", "pvh", "pvh or bzimage")
		fs.StringVar(&o.Suite, "suite", "default", "guest suite")
		fs.StringVar(&o.Name, "name", "", "log name")
		fs.Parse(args[1:])
		o.Extra = fs.Args()
		return BootGuest(c, o)
	case "host-selftest":
		return HostSelftest(c)
	}
	return fmt.Errorf("boot: unknown target %q", args[0])
}

func runSub(name string, args []string) (bool, error) {
	c := LoadConfig()
	switch name {
	case "build":
		return true, cmdBuild(args)
	case "boot":
		return true, cmdBoot(args)
	case "regress":
		return true, Regress(c)
	case "deps":
		return true, Deps(c)
	case "sanitize":
		quiet := len(args) > 0 && args[0] == "-q"
		if quiet {
			args = args[1:]
		}
		return true, Sanitize(c, args, quiet)
	case "selftests":
		if len(args) != 2 {
			return true, fmt.Errorf("pvmtest selftests <log> <vendor>")
		}
		return true, CheckSelftests(c, args[0], args[1])
	case "exits":
		if len(args) < 1 {
			return true, fmt.Errorf("pvmtest exits <case> [vendor...]")
		}
		return true, ExitProfile(c, args[0], args[1:])
	case "resolve-exits":
		if len(args) < 1 {
			return true, fmt.Errorf("pvmtest resolve-exits <log> [vmlinux]")
		}
		vm := ""
		if len(args) > 1 {
			vm = args[1]
		}
		return true, ResolveExits(c, args[0], vm)
	}
	return false, nil
}
