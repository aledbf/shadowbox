package tests

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
	"github.com/aledbf/pvm-testbed/initrd/internal/sysinit"
)

func registerBoot(h *harness.Harness) {
	// If this one fails nothing else is worth reading.
	h.Add(harness.Case{
		Name:   "boot/alive",
		Suites: []string{harness.Smoke},
		Fn: func(t *harness.T) error {
			var u syscall.Utsname
			if err := syscall.Uname(&u); err != nil {
				return err
			}
			t.Logf("pid=%d ppid=%d cpus=%d", os.Getpid(), os.Getppid(), runtime.NumCPU())
			return nil
		},
	})

	h.Add(harness.Case{
		Name:   "boot/procfs",
		Suites: []string{harness.Smoke},
		Fn: func(t *harness.T) error {
			for _, p := range []string{"/proc/cmdline", "/proc/cpuinfo", "/proc/self/maps", "/proc/meminfo"} {
				b, err := os.ReadFile(p)
				if err != nil {
					return fmt.Errorf("read %s: %w", p, err)
				}
				if len(b) == 0 {
					return fmt.Errorf("%s is empty", p)
				}
			}
			return nil
		},
	})

	// A guest that came up with fewer CPUs than it was given did not
	// finish SMP bringup, which under PVM means a vCPU never made it
	// through the switcher.  That is worth its own line in the log.
	h.Add(harness.Case{
		Name:   "boot/smp",
		Suites: []string{harness.Smoke},
		Fn: func(t *harness.T) error {
			online, err := os.ReadFile("/sys/devices/system/cpu/online")
			if err != nil {
				return err
			}
			t.Logf("cpu online: %s", strings.TrimSpace(string(online)))
			if runtime.NumCPU() < 1 {
				return fmt.Errorf("no usable CPU")
			}
			b, err := os.ReadFile("/proc/cpuinfo")
			if err != nil {
				return err
			}
			n := strings.Count(string(b), "processor\t:")
			t.Logf("cpuinfo reports %d processors, runtime sees %d", n, runtime.NumCPU())
			if n != runtime.NumCPU() {
				return fmt.Errorf("cpuinfo says %d processors but the scheduler sees %d",
					n, runtime.NumCPU())
			}
			return nil
		},
	})

	h.Add(harness.Case{
		Name:   "boot/no-oops",
		Suites: []string{harness.Smoke},
		Fn: func(t *harness.T) error {
			b, err := os.ReadFile("/dev/kmsg")
			if err != nil {
				// Reading /dev/kmsg blocks at the end of the buffer, so
				// this is best effort; not having it is not a failure.
				t.Logf("cannot read /dev/kmsg: %v", err)
				return nil
			}
			for _, bad := range []string{"BUG:", "Oops", "WARNING:", "general protection fault", "unable to handle"} {
				if strings.Contains(string(b), bad) {
					return fmt.Errorf("the kernel log contains %q", bad)
				}
			}
			return nil
		},
	})

	h.Add(harness.Case{
		Name:   "boot/exec",
		Suites: []string{harness.Smoke},
		Fn: func(t *harness.T) error {
			// exec of ourselves: page faults, ELF load, and a return to
			// user space through whatever entry path this kernel has.
			code, _, err := runVictim("victim-exit")
			if err != nil {
				return err
			}
			if code != 42 {
				return fmt.Errorf("child exited %d, want 42", code)
			}
			_ = sysinit.Hypervisor()
			return nil
		},
	})
}
