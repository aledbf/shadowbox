package tests

import (
	"fmt"
	"time"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

func registerTime(h *harness.Harness) {
	// A PVM guest reads the TSC directly and gets its offset from the
	// host.  Time going backwards after a migration between physical
	// CPUs is the classic symptom, and it is silent unless something
	// looks.
	h.Add(harness.Case{
		Name:   "time/monotonic",
		Suites: []string{harness.Smoke},
		Fn: func(t *harness.T) error {
			last := time.Now()
			var worst time.Duration
			for i := 0; i < 2_000_000; i++ {
				now := time.Now()
				if d := now.Sub(last); d < 0 {
					return fmt.Errorf("the monotonic clock went backwards by %s "+
						"on iteration %d", -d, i)
				} else if d > worst {
					worst = d
				}
				last = now
			}
			t.Logf("largest gap between consecutive reads: %s", worst)
			return nil
		},
	})

	h.Add(harness.Case{
		Name:   "time/sleep",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			for _, want := range []time.Duration{
				time.Millisecond, 10 * time.Millisecond, 100 * time.Millisecond,
			} {
				start := time.Now()
				time.Sleep(want)
				got := time.Since(start)
				if got < want {
					return fmt.Errorf("sleep(%s) returned after only %s", want, got)
				}
				// A wildly long sleep means timer interrupts are not
				// being injected promptly, which is a host side problem.
				if got > want*20+50*time.Millisecond {
					return fmt.Errorf("sleep(%s) took %s", want, got)
				}
				t.Logf("sleep(%s) took %s", want, got.Round(time.Microsecond))
			}
			return nil
		},
	})

	// Time must be consistent across vCPUs, not just within one.
	h.Add(harness.Case{
		Name:   "time/cross-cpu",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			var prev time.Time
			return onEachCPU(func(cpu int) error {
				now := time.Now()
				if !prev.IsZero() && now.Before(prev) {
					return fmt.Errorf("time on cpu %d is %s behind the previous cpu",
						cpu, prev.Sub(now))
				}
				prev = now
				return nil
			})
		},
	})
}
