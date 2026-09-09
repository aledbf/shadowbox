package tests

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

// The rest of the perf suite is single threaded, which measures the cost of
// one vCPU's exits and says nothing about what happens when several vCPUs
// ask the shadow MMU for work at once.  That is where PVM's design has to
// pay -- address space creation and TLB invalidation are per-VM state -- and
// it is where the two use-after-free bugs the original series fixed lived.
//
// These scale with the number of CPUs the guest was given, so the same case
// run at 1, 2, 8 and 16 vCPUs is a scaling curve rather than a single point.
// pinWorker puts the calling goroutine's thread on one CPU and keeps it
// there.  runtime.NumCPU() only says how many there are: without this the Go
// scheduler is free to run every worker on a couple of CPUs, which measures
// the guest's scheduler rather than the host's shadow MMU.
func pinWorker(cpu int) error {
	runtime.LockOSThread()

	var set cpuSet
	set.set(cpu)
	if err := schedSetaffinity(0, &set); err != nil {
		return fmt.Errorf("pinning to cpu %d: %w", cpu, err)
	}
	return nil
}

func registerParallel(h *harness.Harness) {
	h.Add(harness.Case{
		Name:    "perf/parallel-fork",
		Suites:  []string{harness.Perf},
		Timeout: 600 * 1e9,
		Fn: func(t *harness.T) error {
			n := runtime.NumCPU()
			const perCPU = 60

			// Without this the runtime may keep fewer threads than
			// CPUs and the workers serialise on them.
			defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(n))

			start := time.Now()
			var wg sync.WaitGroup
			errs := make([]error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if err := pinWorker(i); err != nil {
						errs[i] = err
						return
					}
					for j := 0; j < perCPU; j++ {
						code, _, err := runVictim("victim-exit")
						if err != nil {
							errs[i] = err
							return
						}
						if code != 42 {
							errs[i] = fmt.Errorf("child exited %d", code)
							return
						}
					}
				}(i)
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					return err
				}
			}
			d := time.Since(start)
			total := n * perCPU
			t.Logf("%d cpus x %d", n, perCPU)
			t.Metric("us_per_fork_exec", float64(d.Microseconds())/float64(total), "us")
			t.Metric("cpus", float64(n), "count")
			return nil
		},
	})

	// Address space churn from every CPU at once: each worker maps, touches
	// and unmaps, so the host is building and tearing down shadow page
	// tables concurrently and invalidating across vCPUs.
	h.Add(harness.Case{
		Name:    "perf/parallel-fault",
		Suites:  []string{harness.Perf},
		Timeout: 600 * 1e9,
		Fn: func(t *harness.T) error {
			n := runtime.NumCPU()
			const size = 16 << 20
			const rounds = 6
			page := syscall.Getpagesize()

			defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(n))

			start := time.Now()
			var wg sync.WaitGroup
			errs := make([]error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if err := pinWorker(i); err != nil {
						errs[i] = err
						return
					}
					for r := 0; r < rounds; r++ {
						b, err := syscall.Mmap(-1, 0, size,
							syscall.PROT_READ|syscall.PROT_WRITE,
							syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
						if err != nil {
							errs[i] = err
							return
						}
						for off := 0; off < size; off += page {
							b[off] = byte(r)
						}
						for off := 0; off < size; off += page {
							if b[off] != byte(r) {
								errs[i] = fmt.Errorf(
									"worker %d round %d: page +%#x holds %#x",
									i, r, off, b[off])
								_ = syscall.Munmap(b)
								return
							}
						}
						if err := syscall.Munmap(b); err != nil {
							errs[i] = err
							return
						}
					}
				}(i)
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					return err
				}
			}
			d := time.Since(start)
			pages := n * rounds * (size / page)
			// Not ns_per_fault: the time covers mmap, the touch, the
			// read-back check and munmap.  That is the right thing to
			// measure for address space churn, but calling it a fault
			// would invite comparison with perf/page-fault, which
			// measures only the touch.
			t.Logf("%d cpus x %d rounds x %dMB, per page: map+touch+verify+unmap",
				n, rounds, size>>20)
			t.Metric("ns_per_page_cycle", float64(d.Nanoseconds())/float64(pages), "ns")
			return nil
		},
	})
}
