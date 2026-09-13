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

// fault-scaling: first-touch faults and nothing else, from every CPU at once.
//
// parallel-fault times mmap, the touch, a read-back pass and munmap together,
// and munmap from several threads of one process is a TLB shootdown to every
// CPU the process runs on -- IPIs and remote flushes that grow with the CPU
// count by themselves.  Its curve cannot say whether first-touch faults scale.
// This one maps every region first, lines the workers up on a barrier, times
// only the loop that writes one byte to each fresh page, lines them up again,
// and unmaps outside the clock.  One region per worker, so no two vCPUs fault
// the same guest page table.
//
// ns_per_page is wall time over all pages: with perfect scaling it falls as
// 1/cpus.  ns_per_page_per_cpu is the same thing multiplied back by the CPU
// count, so perfect scaling reads as a flat line and the loss is the slope.
func registerFaultScaling(h *harness.Harness) {
	h.Add(harness.Case{
		Name:    "perf/fault-scaling",
		Suites:  []string{harness.Perf},
		Timeout: 600 * 1e9,
		Fn: func(t *harness.T) error {
			n := runtime.NumCPU()
			pages := harness.N(8192)
			const rounds = 4
			page := syscall.Getpagesize()

			defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(n))

			var total time.Duration
			for r := 0; r < rounds; r++ {
				regions := make([][]byte, n)
				for i := range regions {
					b, err := syscall.Mmap(-1, 0, pages*page,
						syscall.PROT_READ|syscall.PROT_WRITE,
						syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
					if err != nil {
						return err
					}
					regions[i] = b
				}

				var ready, done sync.WaitGroup
				start := make(chan struct{})
				errs := make([]error, n)
				ready.Add(n)
				done.Add(n)
				for i := 0; i < n; i++ {
					go func(i int) {
						defer done.Done()
						if err := pinWorker(i); err != nil {
							errs[i] = err
							ready.Done()
							return
						}
						ready.Done()
						<-start
						b := regions[i]
						for off := 0; off < len(b); off += page {
							b[off] = 1
						}
					}(i)
				}
				ready.Wait()
				t0 := time.Now()
				close(start)
				done.Wait()
				total += time.Since(t0)

				for _, b := range regions {
					_ = syscall.Munmap(b)
				}
				for _, err := range errs {
					if err != nil {
						return err
					}
				}
			}

			all := float64(n * pages * rounds)
			t.Logf("%d cpus x %d pages x %d rounds, first touch only", n, pages, rounds)
			t.Metric("ns_per_page", float64(total.Nanoseconds())/all, "ns")
			t.Metric("ns_per_page_per_cpu", float64(total.Nanoseconds())*float64(n)/all, "ns")
			return nil
		},
	})
}

// unmap-scaling: the other half of parallel-fault.  Every worker maps and
// touches its region outside the clock; then all of them unmap at once, and
// only that is timed.  One process, so each munmap is a TLB shootdown to every
// CPU the process runs on, and on a shadow-paging host it is also the
// hypervisor zapping the shadow of every page -- the work fault-scaling leaves
// out.
func registerUnmapScaling(h *harness.Harness) {
	h.Add(harness.Case{
		Name:    "perf/unmap-scaling",
		Suites:  []string{harness.Perf},
		Timeout: 600 * 1e9,
		Fn: func(t *harness.T) error {
			n := runtime.NumCPU()
			pages := harness.N(4096)
			const rounds = 6
			page := syscall.Getpagesize()

			defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(n))

			var total time.Duration
			for r := 0; r < rounds; r++ {
				var ready, done sync.WaitGroup
				start := make(chan struct{})
				errs := make([]error, n)
				ready.Add(n)
				done.Add(n)
				for i := 0; i < n; i++ {
					go func(i int) {
						defer done.Done()
						if err := pinWorker(i); err != nil {
							errs[i] = err
							ready.Done()
							return
						}
						b, err := syscall.Mmap(-1, 0, pages*page,
							syscall.PROT_READ|syscall.PROT_WRITE,
							syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
						if err != nil {
							errs[i] = err
							ready.Done()
							return
						}
						for off := 0; off < len(b); off += page {
							b[off] = 1
						}
						ready.Done()
						<-start
						errs[i] = syscall.Munmap(b)
					}(i)
				}
				ready.Wait()
				t0 := time.Now()
				close(start)
				done.Wait()
				total += time.Since(t0)
				for _, err := range errs {
					if err != nil {
						return err
					}
				}
			}

			all := float64(n * pages * rounds)
			t.Logf("%d cpus x %d pages x %d rounds, munmap only", n, pages, rounds)
			t.Metric("ns_per_page", float64(total.Nanoseconds())/all, "ns")
			t.Metric("ns_per_page_per_cpu", float64(total.Nanoseconds())*float64(n)/all, "ns")
			return nil
		},
	})
}
