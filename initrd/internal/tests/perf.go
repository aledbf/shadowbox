package tests

import (
	"os"
	"syscall"
	"time"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

// The perf cases measure and print; they never fail on a threshold.  A
// number only means something next to another number from the same
// machine, so the comparing is done outside, against a baseline run of
// the same guest on ordinary KVM.  Baking a limit in here would turn a
// slow host into a red test and teach everyone to ignore it.
func registerPerf(h *harness.Harness) {
	h.Add(harness.Case{
		Name:    "perf/syscall",
		Suites:  []string{harness.Perf},
		Timeout: 120 * 1e9,
		Fn: func(t *harness.T) error {
			const n = 2_000_000
			start := time.Now()
			for i := 0; i < n; i++ {
				syscall.Getpid()
			}
			d := time.Since(start)
			t.Metric("ns_per_getpid", float64(d.Nanoseconds())/float64(n), "ns")
			return nil
		},
	})

	h.Add(harness.Case{
		Name:    "perf/page-fault",
		Suites:  []string{harness.Perf},
		Timeout: 120 * 1e9,
		Fn: func(t *harness.T) error {
			const size = 256 << 20
			page := os.Getpagesize()
			b, err := syscall.Mmap(-1, 0, size,
				syscall.PROT_READ|syscall.PROT_WRITE,
				syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
			if err != nil {
				return err
			}
			defer syscall.Munmap(b)

			start := time.Now()
			for i := 0; i < size; i += page {
				b[i] = 1
			}
			d := time.Since(start)
			t.Metric("ns_per_fault", float64(d.Nanoseconds())/float64(size/page), "ns")
			return nil
		},
	})

	h.Add(harness.Case{
		Name:    "perf/fork-exec",
		Suites:  []string{harness.Perf},
		Timeout: 300 * 1e9,
		Fn: func(t *harness.T) error {
			const n = 200
			start := time.Now()
			for i := 0; i < n; i++ {
				if _, _, err := runVictim("victim-exit"); err != nil {
					return err
				}
			}
			d := time.Since(start)
			t.Metric("us_per_fork_exec", float64(d.Microseconds())/float64(n), "us")
			return nil
		},
	})

	h.Add(harness.Case{
		Name:    "perf/context-switch",
		Suites:  []string{harness.Perf},
		Timeout: 120 * 1e9,
		Fn: func(t *harness.T) error {
			const n = 200_000
			ping := make(chan struct{})
			pong := make(chan struct{})
			go func() {
				for i := 0; i < n; i++ {
					<-ping
					pong <- struct{}{}
				}
			}()
			start := time.Now()
			for i := 0; i < n; i++ {
				ping <- struct{}{}
				<-pong
			}
			d := time.Since(start)
			t.Metric("ns_per_roundtrip", float64(d.Nanoseconds())/float64(n), "ns")
			return nil
		},
	})
}
