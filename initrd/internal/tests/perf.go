package tests

import (
	"fmt"
	"io"
	"os"
	"runtime"
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

			// Without this the result is bimodal and useless.  A
			// transparent huge page faults in 512 base pages at
			// once, so whether khugepaged and the fault path
			// happen to give this mapping THP changes the answer
			// by tens of percent -- between two sweeps of the same
			// build it moved 24%, which is larger than anything
			// worth measuring.  Ask for base pages and measure
			// base-page faults.
			if err := syscall.Madvise(b, madvNoHugepage); err != nil {
				t.Logf("MADV_NOHUGEPAGE: %v (result will be noisy)", err)
			}

			start := time.Now()
			for i := 0; i < size; i += page {
				b[i] = 1
			}
			d := time.Since(start)
			t.Logf("%d base-page faults, THP suppressed", size/page)
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

	// Two processes on one CPU, passing a byte back and forth through a
	// pair of pipes.  Each roundtrip is two real context switches,
	// because neither side can proceed until the other has run and there
	// is only one CPU for them to run on.
	//
	// This replaces a version that ping-ponged between two goroutines
	// over unbuffered channels.  That measured the Go scheduler, not the
	// kernel: depending on GOMAXPROCS and whether a spinning P happened
	// to pick the work up, a roundtrip was sometimes a userspace
	// goroutine switch and sometimes a futex sleep and wake.  The two
	// differ by an order of magnitude, and the result varied by 116%
	// across five runs of the same build -- enough to invent a 91%
	// regression that did not exist.  The same mistake as measuring
	// signal delivery through a Go channel.
	h.Add(harness.Case{
		Name:    "perf/context-switch",
		Suites:  []string{harness.Perf},
		Timeout: 180 * 1e9,
		Fn: func(t *harness.T) error {
			const n = 50_000

			toChild, err := pipePair()
			if err != nil {
				return err
			}
			defer toChild.close()
			toParent, err := pipePair()
			if err != nil {
				return err
			}
			defer toParent.close()

			// Both ends on CPU 0, so the kernel has to switch
			// between them rather than running them side by side.
			cmd, err := startVictimPinned("victim-pingpong",
				[]*os.File{toChild.r, toParent.w}, 0)
			if err != nil {
				return err
			}
			defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			var set cpuSet
			set.set(0)
			if err := schedSetaffinity(0, &set); err != nil {
				return fmt.Errorf("pinning the parent: %w", err)
			}

			buf := []byte{0}
			// One warm-up roundtrip: the child has to be scheduled
			// and its pages faulted in before the clock starts.
			if _, err := toChild.w.Write(buf); err != nil {
				return err
			}
			if _, err := io.ReadFull(toParent.r, buf); err != nil {
				return fmt.Errorf("the child never answered: %w", err)
			}

			start := time.Now()
			for i := 0; i < n; i++ {
				if _, err := toChild.w.Write(buf); err != nil {
					return err
				}
				if _, err := io.ReadFull(toParent.r, buf); err != nil {
					return err
				}
			}
			d := time.Since(start)
			t.Logf("%d roundtrips between two processes pinned to cpu 0, "+
				"two context switches each", n)
			t.Metric("ns_per_pipe_roundtrip",
				float64(d.Nanoseconds())/float64(n), "ns")
			return nil
		},
	})
}
