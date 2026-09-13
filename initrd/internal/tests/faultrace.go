package tests

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

// mm/fault-race: guest page table entries going absent and present under
// faults from other vCPUs on the same pages.
//
// One writer fills a small region with an odd marker and zaps it with
// MADV_DONTNEED, over and over; every other CPU reads it the whole time.  A
// reader faults on entries that are absent, or were absent when its fault
// was taken and are present by the time it is handled, or were present and
// lost their SPTE to the zap -- the cases a hypervisor that answers a user
// #PF without walking the guest page tables (kvm-pvm direct_pf) gets wrong
// by delivering a fault the guest cannot fix or by not letting the MMU fix
// one.  Wrong shows up as a reader stuck on a page (the case times out), a
// signal (the process dies), or a byte that is neither zero nor a marker.
func registerFaultRace(h *harness.Harness) {
	h.Add(harness.Case{
		Name:    "mm/fault-race",
		Suites:  []string{harness.Core},
		Timeout: 120 * 1e9,
		Fn: func(t *harness.T) error {
			const pages = 64
			const cycles = 2000
			page := syscall.Getpagesize()
			n := runtime.NumCPU()
			if n < 2 {
				n = 2
			}
			defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(n))

			b, err := syscall.Mmap(-1, 0, pages*page,
				syscall.PROT_READ|syscall.PROT_WRITE,
				syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
			if err != nil {
				return err
			}
			defer syscall.Munmap(b)

			var stop atomic.Bool
			var reads atomic.Uint64
			errs := make([]error, n)
			var wg sync.WaitGroup
			for r := 1; r < n; r++ {
				wg.Add(1)
				go func(r int) {
					defer wg.Done()
					runtime.LockOSThread()
					for !stop.Load() {
						for p := 0; p < pages; p++ {
							v := b[p*page+r%page]
							if v != 0 && v&1 == 0 {
								errs[r] = fmt.Errorf("reader %d: page %d holds %#x, "+
									"neither zero nor a marker", r, p, v)
								stop.Store(true)
								return
							}
						}
						reads.Add(pages)
					}
				}(r)
			}

			start := time.Now()
			for c := 0; c < cycles && !stop.Load(); c++ {
				mark := byte(c<<1 | 1)
				for p := 0; p < pages; p++ {
					for i := 0; i < n; i++ {
						b[p*page+i%page] = mark
					}
				}
				if err := syscall.Madvise(b, syscall.MADV_DONTNEED); err != nil {
					errs[0] = fmt.Errorf("madvise: %w", err)
					break
				}
			}
			stop.Store(true)
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					return err
				}
			}
			t.Logf("%d zap cycles of %d pages, %d reads from %d CPUs in %s",
				cycles, pages, reads.Load(), n-1, time.Since(start).Round(time.Millisecond))
			return nil
		},
	})
}
