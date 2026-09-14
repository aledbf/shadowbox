package main

import (
	"bufio"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Config is what every command reads from the environment.  The variables
// are the ones the testbed's scripts have always honoured, with the same
// defaults, so a habit like "L1_MEM=12G make stage2" keeps working.
type Config struct {
	Testbed     string
	Out         string // OUT
	Cache       string // CACHE
	KSRC        string // KSRC: the kernel tree under test; the testbed has none
	Jobs        int    // JOBS
	Qemu        string // QEMU
	GuestCPUs   string // GUEST_CPUS
	GuestMem    string // GUEST_MEM
	L1CPUs      string // L1_CPUS
	L1Mem       string // L1_MEM
	BootTimeout int    // BOOT_TIMEOUT, seconds
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func LoadConfig() Config {
	tb := testbedDir()
	jobs, err := strconv.Atoi(os.Getenv("JOBS"))
	if err != nil || jobs <= 0 {
		jobs = runtime.NumCPU()
	}
	bt, err := strconv.Atoi(os.Getenv("BOOT_TIMEOUT"))
	if err != nil || bt <= 0 {
		bt = 180
	}
	return Config{
		Testbed:     tb,
		Out:         envOr("OUT", filepath.Join(tb, "out")),
		Cache:       envOr("CACHE", filepath.Join(tb, "cache")),
		KSRC:        os.Getenv("KSRC"),
		Jobs:        jobs,
		Qemu:        envOr("QEMU", "qemu-system-x86_64"),
		GuestCPUs:   envOr("GUEST_CPUS", "2"),
		GuestMem:    envOr("GUEST_MEM", "1G"),
		L1CPUs:      envOr("L1_CPUS", "8"),
		L1Mem:       envOr("L1_MEM", "8G"),
		BootTimeout: bt,
	}
}

func (c Config) NeedKSRC() error {
	if c.KSRC == "" {
		return errors.New("KSRC must name a kernel tree (the testbed carries none)")
	}
	if st, err := os.Stat(c.KSRC); err != nil || !st.IsDir() {
		return fmt.Errorf("KSRC=%s is not a directory", c.KSRC)
	}
	return nil
}

// With returns a copy of c whose out/ and kernel tree are the given ones.
func (c Config) With(out, ksrc string) Config {
	if out != "" {
		c.Out = out
	}
	if ksrc != "" {
		c.KSRC = ksrc
	}
	return c
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\033[1;34m==>\033[0m "+format+"\n", a...)
}

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\033[1;33m warn\033[0m "+format+"\n", a...)
}

// need fails when a tool is missing, saying where to get it.
func need(tool, hint string) error {
	if _, err := exec.LookPath(tool); err != nil {
		if hint != "" {
			return fmt.Errorf("missing tool: %s  (%s)", tool, hint)
		}
		return fmt.Errorf("missing tool: %s", tool)
	}
	return nil
}

func needRun(c Config) error {
	if err := need(c.Qemu, "Debian/Ubuntu: apt install qemu-system-x86"); err != nil {
		return err
	}
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("/dev/kvm is not writable: add yourself to the kvm group")
	}
	f.Close()
	return nil
}

// PinCPUList is which CPUs to run a measured VM on.
//
// A hybrid CPU is the single largest source of run-to-run noise here: this
// machine has six P-cores at 5.2-5.4GHz and eight E-cores at 4.1GHz, and
// where the scheduler happens to put qemu's threads changes the answer by
// tens of percent.  Two sweeps of the same build read 14810 and 10020 ns on
// the same benchmark before this existed.
//
// So pin to the fastest set of CPUs, detected rather than hardcoded:
// whichever share the highest maximum frequency.  On a uniform machine that
// is all of them and the pinning is a no-op.
//
//	PIN_CPUS=0-11   use exactly this list
//	PIN_CPUS=none   do not pin at all
func PinCPUList() string {
	switch v := os.Getenv("PIN_CPUS"); v {
	case "none":
		return ""
	case "":
	default:
		return v
	}
	type cpu struct {
		id  int
		mhz float64
	}
	var cpus []cpu
	top := 0.0
	dirs, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*")
	for _, d := range dirs {
		id, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(d), "cpu"))
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(d, "cpufreq", "cpuinfo_max_freq"))
		if err != nil {
			continue
		}
		khz, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
		if err != nil {
			continue
		}
		cpus = append(cpus, cpu{id, khz})
		if khz > top {
			top = khz
		}
	}
	if top == 0 {
		return ""
	}
	sort.Slice(cpus, func(i, j int) bool { return cpus[i].id < cpus[j].id })
	var fast []string
	for _, c := range cpus {
		// 0.9 separates core *types*, not turbo bins: on this machine
		// the E-cores are at 76% of the top P-core and the slower
		// P-cores at 96%, so anything in between would keep two cores
		// and drop ten.
		if c.mhz >= top*0.9 {
			fast = append(fast, strconv.Itoa(c.id))
		}
	}
	// All of them means there is nothing to choose between.
	if len(fast) <= 1 || len(fast) >= len(cpus) {
		return ""
	}
	return strings.Join(fast, ",")
}

// CheckStatic fails unless path is an ELF executable with no program
// interpreter and no shared library dependencies: every Go binary the
// testbed ships -- this one, the guest init, the L1 agent, the host tests --
// runs on a system that may not have the libc it was linked against, or any.
func CheckStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s is dynamically linked (has a program interpreter)", path)
		}
	}
	libs, err := f.ImportedLibraries()
	if err == nil && len(libs) > 0 {
		return fmt.Errorf("%s needs shared libraries: %s", path, strings.Join(libs, " "))
	}
	return nil
}

// goBuildStatic builds a Go main package into out, static, and proves it.
func goBuildStatic(dir, pkg, out string) error {
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build %s: %w", pkg, err)
	}
	return CheckStatic(out)
}

// run executes a command with its output on stderr, in dir.
func runCmd(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, mode)
}

// readList returns the non-comment, non-blank lines of a config list.
func readList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return out, sc.Err()
}
