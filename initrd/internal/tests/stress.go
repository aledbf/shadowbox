package tests

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

func registerStress(h *harness.Harness) {
	// Process churn: each fork+exec+exit is an address space created and
	// torn down, which for a PVM host means shadow page tables built and
	// freed.  Two of the bugs already fixed in the original series were
	// use-after-free in exactly that path, so this runs long enough to
	// hit it.
	h.Add(harness.Case{
		Name:    "stress/process-churn",
		Suites:  []string{harness.Full},
		Timeout: 300 * 1e9,
		Fn: func(t *harness.T) error {
			const rounds = 300
			const parallel = 8
			start := time.Now()
			for i := 0; i < rounds; i += parallel {
				var wg sync.WaitGroup
				errs := make([]error, parallel)
				for j := 0; j < parallel; j++ {
					wg.Add(1)
					go func(j int) {
						defer wg.Done()
						code, _, err := runVictim("victim-exit")
						if err != nil {
							errs[j] = err
						} else if code != 42 {
							errs[j] = fmt.Errorf("child exited %d, want 42", code)
						}
					}(j)
				}
				wg.Wait()
				for _, err := range errs {
					if err != nil {
						return fmt.Errorf("after %d rounds: %w", i, err)
					}
				}
			}
			d := time.Since(start)
			t.Logf("%d fork+exec+exit in %s (%s each)", rounds,
				d.Round(time.Millisecond), (d / rounds).Round(time.Microsecond))
			return nil
		},
	})

	// Sustained page fault pressure while other threads are running:
	// this is where a stale shadow PTE turns into wrong data rather than
	// a crash, so the data is checked, not just the absence of a fault.
	h.Add(harness.Case{
		Name:    "stress/fault-storm",
		Suites:  []string{harness.Full},
		Timeout: 300 * 1e9,
		Fn: func(t *harness.T) error {
			const workers = 4
			const size = 32 << 20
			page := os.Getpagesize()

			var wg sync.WaitGroup
			errs := make([]error, workers)
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for round := 0; round < 8; round++ {
						b, err := syscall.Mmap(-1, 0, size,
							syscall.PROT_READ|syscall.PROT_WRITE,
							syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
						if err != nil {
							errs[w] = fmt.Errorf("worker %d round %d: mmap: %w", w, round, err)
							return
						}
						mark := byte(w*16 + round)
						for i := 0; i < size; i += page {
							b[i] = mark
						}
						for i := 0; i < size; i += page {
							if b[i] != mark {
								errs[w] = fmt.Errorf(
									"worker %d round %d: page +%#x holds %#x, wrote %#x",
									w, round, i, b[i], mark)
								_ = syscall.Munmap(b)
								return
							}
						}
						if err := syscall.Munmap(b); err != nil {
							errs[w] = fmt.Errorf("worker %d round %d: munmap: %w", w, round, err)
							return
						}
					}
				}(w)
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					return err
				}
			}
			return nil
		},
	})
}
