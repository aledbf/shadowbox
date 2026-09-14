package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// L1Opts is one boot of L1 -- the PVM host -- as an ordinary KVM guest of
// this machine, with the guest kernel handed in over 9p.  L1 needs no nested
// VMX: a PVM host uses none, which is the property this whole harness exists
// to check.
//
// It builds nothing.  It boots whatever host kernel, guest kernel and initrd
// are already in c.Out, so after editing the kernel build first; skipping
// that is how a measurement got taken against a module that did not contain
// the change being measured.
type L1Opts struct {
	Suite   string   // pvmtest.suite; "profile", "lock", "mmu", "hosttests", "failclosed" are agent modes
	Vendor  string   // pvm or intel: which KVM vendor L1 loads
	Guest   []string // extra guest arguments (pvmtest.guest_append)
	Mod     []string // vendor module arguments
	CPUs    string   // guest vCPUs; "" is the agent's default
	Mem     string   // guest memory; "" is the agent's default
	Profile string   // pvmtest.profile_case
	L1Args  []string // extra L1 kernel arguments
	Accel   string   // kvm (default) or tcg
	CPU     string   // L1 -cpu, default host
	Timeout time.Duration
	Log     string // where the serial log goes
	Echo    bool   // also copy the log to stdout
	Pin     bool   // pin to the fast cores (measurements)
	QMP     string // L1_QMP: a QMP socket, to ask a wedged L1 where its CPUs are
	Debug   string // L1_QEMU_DEBUG: qemu -d flags
}

// Tee target that can be nil.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// stagePayload fills a fresh directory with what L1 needs from us, and
// nothing else.  A fresh directory each run, gone afterwards: a fixed path
// under out/ that gets removed at the start is one sudo run away from
// breaking every unprivileged run after it.
func stagePayload(c Config, dir string) error {
	img := filepath.Join(c.Out, "images")
	for _, f := range []string{"guest-vmlinux", "initrd.cpio.gz"} {
		if err := copyFile(filepath.Join(img, f), filepath.Join(dir, f), 0o644); err != nil {
			return err
		}
	}
	agent := filepath.Join(c.Out, "l1agent")
	if !fileExists(agent) {
		return fmt.Errorf("no L1 agent at %s -- pvmtest build agent", agent)
	}
	if err := CheckStatic(agent); err != nil {
		return err
	}
	if err := copyFile(agent, filepath.Join(dir, "l1agent"), 0o755); err != nil {
		return err
	}
	// The host-side tests run in L1 against the loaded vendor module;
	// nothing about them needs the guest.
	if err := copyTree(filepath.Join(c.Out, "hosttests"), filepath.Join(dir, "hosttests"), ""); err != nil {
		return err
	}
	// SELFTESTS=<glob> stages only the matching ones.  A full sweep is
	// twenty-odd minutes, which is a long time to wait when the question is
	// about one of them.
	if err := copyTree(filepath.Join(c.Out, "kvm-selftests"), filepath.Join(dir, "kvm-selftests"), os.Getenv("SELFTESTS")); err != nil {
		return err
	}
	// The KVM modules travel with the payload rather than in the image: the
	// rootfs is bootstrapped once, and the kernel's version string -- and so
	// the path modprobe would look under -- changes with every commit.
	filepath.WalkDir(filepath.Join(c.Out, "modules-host"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "kvm") && strings.Contains(d.Name(), ".ko") {
			copyFile(p, filepath.Join(dir, d.Name()), 0o644)
		}
		return nil
	})
	// perf, if one has been built against this tree.  L1's own distro perf
	// would not know the PVM exit reasons.  A lean perf: "perf kvm stat"
	// needs libtraceevent and libdw makes its symbols readable, and those
	// two libraries travel alongside it.
	perf := envOr("PERF", filepath.Join(c.Testbed, "..", "build-perf-lean", "perf"))
	if st, err := os.Stat(perf); err == nil && st.Mode()&0o111 != 0 {
		copyFile(perf, filepath.Join(dir, "perf"), 0o755)
		for _, lib := range []string{"libtraceevent.so.1", "libdw.so.1"} {
			if p := ldconfigPath(lib); p != "" {
				copyFile(p, filepath.Join(dir, lib), 0o644)
			}
		}
		// perf annotate needs the symbols; host-vmlinux keeps its symtab.
		if h := filepath.Join(img, "host-vmlinux"); fileExists(h) {
			copyFile(h, filepath.Join(dir, "host-vmlinux"), 0o644)
		}
	}
	return nil
}

func copyTree(from, to, glob string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return nil // nothing to stage
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, e.Name()); !ok {
				continue
			}
		}
		if err := copyFile(filepath.Join(from, e.Name()), filepath.Join(to, e.Name()), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func ldconfigPath(lib string) string {
	out, err := exec.Command("ldconfig", "-p").Output()
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) >= 4 && f[0] == lib {
			return f[len(f)-1]
		}
	}
	return ""
}

// runQemu runs a qemu command line with the serial output copied to log (and
// echo), bounded by timeout: SIGTERM to the whole process group, SIGKILL five
// seconds later.  Returns whether it timed out.
func runQemu(argv []string, log string, echo io.Writer, timeout time.Duration) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(log), 0o755); err != nil {
		return false, err
	}
	lf, err := os.Create(log)
	if err != nil {
		return false, err
	}
	defer lf.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = io.MultiWriter(lf, echo)
	cmd.Stderr = cmd.Stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case err := <-done:
		return false, err
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		return true, nil
	}
}

// BootL1 boots L1 once and returns the path of its log.  The verdict is the
// caller's (ParseLog/judge); errors are for a boot that could not happen or
// timed out.
func BootL1(c Config, o L1Opts) (string, error) {
	if err := needRun(c); err != nil {
		return "", err
	}
	img := filepath.Join(c.Out, "images")
	if !fileExists(filepath.Join(img, "host-bzImage")) {
		return "", fmt.Errorf("no host kernel in %s -- build it first", img)
	}
	if !fileExists(filepath.Join(img, "l1-rootfs.ext4")) {
		return "", fmt.Errorf("no L1 rootfs in %s -- pvmtest build rootfs", img)
	}
	if o.Suite == "" {
		o.Suite = "default"
	}
	if o.Vendor == "" {
		o.Vendor = "pvm"
	}
	payload, err := os.MkdirTemp("", "pvm-payload.")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(payload)
	if err := stagePayload(c, payload); err != nil {
		return "", err
	}
	if o.Log == "" {
		o.Log = filepath.Join(c.Out, "logs", fmt.Sprintf("l1-%s-%s.log", o.Suite, o.Vendor))
	}
	if o.Timeout == 0 {
		o.Timeout = time.Duration(3*c.BootTimeout) * time.Second
		switch o.Suite {
		case "full", "perf", "all", "profile", "lock", "mmu":
			o.Timeout = 2400 * time.Second
		}
	}
	if o.Accel == "" {
		o.Accel = "kvm"
	}
	if o.CPU == "" {
		o.CPU = "host"
	}
	argv := L1Argv(c, o, payload)
	echo := io.Writer(nopWriter{})
	if o.Echo {
		echo = os.Stdout
	}
	logf("booting L1 (KVM vendor=%s), suite=%s", o.Vendor, o.Suite)
	timedOut, err := runQemu(argv, o.Log, echo, o.Timeout)
	if timedOut {
		return o.Log, fmt.Errorf("L1 timed out after %s -- see %s", o.Timeout, o.Log)
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return o.Log, err
	}
	return o.Log, nil
}

// L1Argv is the qemu command line for one L1 boot.
func L1Argv(c Config, o L1Opts, payload string) []string {
	var argv []string
	// Pinned, so a measured run cannot land on a different class of core
	// than the run it is being compared with.  Never under TCG, which is
	// for correctness only.
	if o.Pin && o.Accel == "kvm" {
		if cpus := PinCPUList(); cpus != "" {
			argv = append(argv, "taskset", "-c", cpus)
		}
	}
	appendLine := []string{"root=/dev/vda", "rw", "console=ttyS0,115200", "panic=-1",
		"pvmtest.suite=" + o.Suite, "pvmtest.vendor=" + o.Vendor}
	if o.Profile != "" {
		appendLine = append(appendLine, "pvmtest.profile_case="+o.Profile)
	}
	if len(o.Guest) > 0 {
		appendLine = append(appendLine, "pvmtest.guest_append="+strings.Join(o.Guest, ","))
	}
	if len(o.Mod) > 0 {
		appendLine = append(appendLine, "pvmtest.mod_args="+strings.Join(o.Mod, ","))
	}
	if o.CPUs != "" {
		appendLine = append(appendLine, "pvmtest.guest_cpus="+o.CPUs)
	}
	if o.Mem != "" {
		appendLine = append(appendLine, "pvmtest.guest_mem="+o.Mem)
	}
	appendLine = append(appendLine, o.L1Args...)
	appendLine = append(appendLine, "systemd.mask=serial-getty@ttyS0.service", "systemd.show_status=false")
	img := filepath.Join(c.Out, "images")
	argv = append(argv, c.Qemu,
		"-machine", "q35,accel="+o.Accel,
		"-cpu", o.CPU,
		"-smp", c.L1CPUs, "-m", c.L1Mem,
		"-kernel", filepath.Join(img, "host-bzImage"),
		// The rootfs is shared by every L1 and booted as a snapshot: L1's
		// writes go to a temporary overlay, so the image is never modified
		// and several L1s can run at once.
		"-drive", "file="+filepath.Join(img, "l1-rootfs.ext4")+",if=virtio,format=raw,snapshot=on",
		"-append", strings.Join(appendLine, " "),
		"-virtfs", "local,path="+payload+",mount_tag=payload,security_model=none,readonly=on")
	if o.Debug != "" {
		argv = append(argv, "-d", o.Debug, "-D", filepath.Join(c.Out, "logs", "l1-qemu-debug.log"))
	}
	if o.QMP != "" {
		argv = append(argv, "-qmp", "unix:"+o.QMP+",server=on,wait=off")
	}
	argv = append(argv, "-nographic", "-no-reboot", "-display", "none", "-serial", "mon:stdio")
	return argv
}

// GuestOpts is one boot of the guest kernel on this machine under ordinary
// KVM: stage 0, and the regression checks.  The command line is the one the
// guest gets inside L1, on purpose -- if the two disagree, the difference is
// PVM.
type GuestOpts struct {
	Boot   string // pvh (the PVH ELF note) or bzimage
	Suite  string
	Name   string
	Kernel string // default: the guest image for Boot
	Extra  []string
	Echo   bool
}

var resultRe0 = regexp.MustCompile(`(?m)^PVMTEST-RESULT: (ok|fail) .*$`)

// BootGuest boots the guest kernel with the Go initrd and returns the
// harness's verdict: a suite can pass every case and still have left a WARN
// in the log behind it, so the sanitizer runs first.
func BootGuest(c Config, o GuestOpts) error {
	if err := needRun(c); err != nil {
		return err
	}
	if o.Boot == "" {
		o.Boot = "pvh"
	}
	if o.Suite == "" {
		o.Suite = "default"
	}
	if o.Name == "" {
		o.Name = "guest-" + o.Boot + "-" + o.Suite
	}
	img := filepath.Join(c.Out, "images")
	kernel := o.Kernel
	if kernel == "" {
		switch o.Boot {
		case "pvh":
			kernel = filepath.Join(img, "guest-vmlinux")
		case "bzimage":
			kernel = filepath.Join(img, "guest-bzImage")
		default:
			return fmt.Errorf("unknown boot mode %q", o.Boot)
		}
	}
	if !fileExists(kernel) {
		return fmt.Errorf("no %s -- build the guest kernel first", kernel)
	}
	initrd := filepath.Join(img, "initrd.cpio.gz")
	if !fileExists(initrd) {
		return fmt.Errorf("no initrd -- pvmtest build initrd")
	}
	log := filepath.Join(c.Out, "logs", o.Name+".log")
	cmdline := "console=ttyS0,115200 earlyprintk=serial,ttyS0,115200 panic=-1 oops=panic no_timer_check " +
		"pvmtest.suite=" + o.Suite + " pvmtest.tag=" + o.Name
	var argv []string
	if cpus := PinCPUList(); cpus != "" {
		argv = append(argv, "taskset", "-c", cpus)
	}
	argv = append(argv, c.Qemu, "-machine", "q35,accel=kvm", "-cpu", "host",
		"-smp", c.GuestCPUs, "-m", c.GuestMem, "-kernel", kernel, "-initrd", initrd,
		"-append", cmdline, "-nographic", "-no-reboot", "-serial", "mon:stdio", "-display", "none")
	argv = append(argv, o.Extra...)
	logf("booting %s (%s)", o.Name, filepath.Base(kernel))
	echo := io.Writer(nopWriter{})
	if o.Echo {
		echo = os.Stdout
	}
	timedOut, _ := runQemu(argv, log, echo, time.Duration(c.BootTimeout)*time.Second)
	if timedOut {
		return fmt.Errorf("%s: timed out after %ds -- see %s", o.Name, c.BootTimeout, log)
	}
	sanErr := Sanitize(c, []string{log}, true)
	raw, _ := os.ReadFile(log)
	m := resultRe0.FindSubmatch(raw)
	switch {
	case m != nil && string(m[1]) == "ok":
		logf("%s: %s", o.Name, strings.TrimSpace(string(m[0])))
		if sanErr != nil {
			return fmt.Errorf("%s: every case passed, but the kernel log did not", o.Name)
		}
		return nil
	case m != nil:
		warnf("%s: %s", o.Name, strings.TrimSpace(string(m[0])))
		return fmt.Errorf("%s failed", o.Name)
	}
	return fmt.Errorf("%s: the guest never reported a result -- see %s", o.Name, log)
}

// HostSelftest boots the host kernel as an ordinary KVM guest, with the same
// initrd the guest uses, purely to find out whether it reaches user space.  A
// change to the PVM host backend that breaks the host kernel's own boot would
// otherwise only show up as an unexplained hang inside L1.
func HostSelftest(c Config) error {
	if err := needRun(c); err != nil {
		return err
	}
	img := filepath.Join(c.Out, "images")
	log := filepath.Join(c.Out, "logs", "host-selftest.log")
	argv := []string{c.Qemu, "-machine", "q35,accel=kvm", "-cpu", "host", "-smp", "2", "-m", "2G",
		"-kernel", filepath.Join(img, "host-bzImage"), "-initrd", filepath.Join(img, "initrd.cpio.gz"),
		"-append", "console=ttyS0,115200 earlyprintk=serial,ttyS0,115200 panic=-1 oops=panic pvmtest.suite=smoke pvmtest.tag=host-selftest",
		"-nographic", "-no-reboot", "-display", "none", "-serial", "mon:stdio"}
	timedOut, _ := runQemu(argv, log, nopWriter{}, time.Duration(c.BootTimeout)*time.Second)
	if timedOut {
		return fmt.Errorf("host kernel timed out -- see %s", log)
	}
	raw, _ := os.ReadFile(log)
	if !regexp.MustCompile(`(?m)^PVMINIT: up`).Match(raw) {
		return fmt.Errorf("the host kernel never reached user space -- see %s", log)
	}
	// kvm_x86_vendor_init() WARNs once for every kvm_x86_ops the PVM
	// backend does not implement: a known, listed gap (configs/log-allow.txt),
	// reported rather than failed on.
	n := len(regexp.MustCompile(`WARNING:.*kvm-x86-(nested-|pmu-)?ops\.h`).FindAll(raw, -1))
	logf("host kernel reached user space; %d missing-kvm_x86_ops warnings", n)
	return Sanitize(c, []string{log}, true)
}

// Regress is what has to pass before anything riskier is worth running.  All
// of it happens here under ordinary KVM, so none of it can wedge anything:
// the kernels under test run as guests of this machine's own stock kernel.
func Regress(c Config) error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"guest / PVH / smoke", func() error { return BootGuest(c, GuestOpts{Boot: "pvh", Suite: "smoke", Name: "regress-pvh-smoke"}) }},
		{"guest / bzImage / smoke", func() error {
			return BootGuest(c, GuestOpts{Boot: "bzimage", Suite: "smoke", Name: "regress-bzimage-smoke"})
		}},
		{"guest / PVH / default", func() error {
			return BootGuest(c, GuestOpts{Boot: "pvh", Suite: "default", Name: "regress-pvh-default"})
		}},
		// The host kernel is booted with the same initrd: reaching user
		// space is exactly what we need to know.
		{"host kernel boots", func() error { return HostSelftest(c) }},
	}
	failed := 0
	for _, s := range steps {
		fmt.Fprintf(os.Stderr, "\n\033[1m--- %s\033[0m\n", s.name)
		if err := s.fn(); err != nil {
			fmt.Fprintf(os.Stderr, "\033[31mFAIL\033[0m %s: %v\n", s.name, err)
			failed++
		} else {
			fmt.Fprintf(os.Stderr, "\033[32mPASS\033[0m %s\n", s.name)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d regression check(s) failed", failed)
	}
	fmt.Fprintf(os.Stderr, "\n\033[32mall regression checks passed\033[0m\n")
	return nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
