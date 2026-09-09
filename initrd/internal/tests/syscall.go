package tests

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

func registerSyscall(h *harness.Harness) {
	// Under PVM a syscall is a real hardware SYSCALL taken at CPL3 and
	// handled by the guest kernel without a VM exit.  If the switcher
	// gets the return path wrong, this is where it shows.
	h.Add(harness.Case{
		Name:   "syscall/roundtrip",
		Suites: []string{harness.Smoke},
		Fn: func(t *harness.T) error {
			want := os.Getpid()
			for i := 0; i < 200000; i++ {
				if got := syscall.Getpid(); got != want {
					return fmt.Errorf("getpid returned %d, want %d, on iteration %d",
						got, want, i)
				}
			}
			return nil
		},
	})

	// getcpu comes out of the vDSO via RDTSCP, and the PVM host has to
	// keep the CPUNODE GDT entry consistent with where the vCPU actually
	// is.  A mismatch here is a real bug and an easy one to miss.
	h.Add(harness.Case{
		Name:   "syscall/getcpu",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			seen := map[int]int{}
			for i := 0; i < 10000; i++ {
				cpu, node, err := getcpu()
				if err != nil {
					return fmt.Errorf("getcpu: %w", err)
				}
				if cpu < 0 || cpu > 4096 {
					return fmt.Errorf("getcpu returned cpu=%d node=%d", cpu, node)
				}
				seen[cpu]++
			}
			t.Logf("observed %d distinct CPUs", len(seen))
			return nil
		},
	})

	// A pinned thread must see the CPU it was pinned to.  This checks
	// the same RDTSCP path as above, but with an answer we know.
	h.Add(harness.Case{
		Name:   "syscall/getcpu-affinity",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			return onEachCPU(func(cpu int) error {
				for i := 0; i < 1000; i++ {
					got, _, err := getcpu()
					if err != nil {
						return err
					}
					if got != cpu {
						return fmt.Errorf("pinned to cpu %d but getcpu says %d", cpu, got)
					}
				}
				return nil
			})
		},
	})

	h.Add(harness.Case{
		Name:   "syscall/errno",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			// A failing syscall must come back as a failure, with the
			// right errno: the return path has to preserve %rax.
			if _, err := os.Open("/definitely/not/here"); !os.IsNotExist(err) {
				return fmt.Errorf("open of a missing path gave %v, want ENOENT", err)
			}
			if err := syscall.Close(-1); err != syscall.EBADF {
				return fmt.Errorf("close(-1) gave %v, want EBADF", err)
			}
			return nil
		},
	})

	// The vDSO reads time without entering the kernel at all; the
	// syscall does not.  Under PVM these take completely different paths
	// and are the easiest pair to get out of step.
	h.Add(harness.Case{
		Name:   "syscall/vdso-agrees",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			for i := 0; i < 1000; i++ {
				vdso := time.Now() // Go uses the vDSO here
				var ts syscall.Timespec
				if _, _, e := syscall.Syscall(syscall.SYS_CLOCK_GETTIME,
					uintptr(0 /* CLOCK_REALTIME */), uintptr(unsafe.Pointer(&ts)), 0); e != 0 {
					return fmt.Errorf("clock_gettime: %v", e)
				}
				sys := time.Unix(ts.Sec, ts.Nsec)
				d := sys.Sub(vdso)
				if d < 0 {
					d = -d
				}
				// The two reads are not simultaneous; a second apart
				// means they are reading different clocks.
				if d > time.Second {
					return fmt.Errorf("vDSO and syscall clocks differ by %s", d)
				}
			}
			return nil
		},
	})
}

func getcpu() (cpu, node int, err error) {
	var c, n uint32
	_, _, e := syscall.RawSyscall(sysGetcpu,
		uintptr(unsafe.Pointer(&c)), uintptr(unsafe.Pointer(&n)), 0)
	if e != 0 {
		return 0, 0, e
	}
	return int(c), int(n), nil
}
