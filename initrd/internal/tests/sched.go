package tests

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

func registerSched(h *harness.Harness) {
	// Every context switch under PVM goes through the switcher, and a
	// thread migrating between vCPUs is what exercises the ASID/PCID
	// partitioning.  Contention is the point, not throughput.
	h.Add(harness.Case{
		Name:   "sched/threads",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			const workers = 64
			const each = 20000
			var counter int64
			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < each; j++ {
						atomic.AddInt64(&counter, 1)
						if j%1000 == 0 {
							runtime.Gosched()
						}
					}
				}()
			}
			wg.Wait()
			if got := atomic.LoadInt64(&counter); got != workers*each {
				return fmt.Errorf("counter is %d, want %d: lost atomic updates",
					got, workers*each)
			}
			return nil
		},
	})

	// Futex wake/wait across CPUs: the wake is an IPI, and an IPI to a
	// vCPU that is not currently on a physical CPU is the interesting
	// case for a PVM host.
	h.Add(harness.Case{
		Name:   "sched/futex",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			const rounds = 20000
			ping := make(chan struct{})
			pong := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				for i := 0; i < rounds; i++ {
					<-ping
					pong <- struct{}{}
				}
				done <- nil
			}()
			start := time.Now()
			for i := 0; i < rounds; i++ {
				ping <- struct{}{}
				<-pong
			}
			<-done
			d := time.Since(start)
			t.Logf("%d round trips in %s (%s each)", rounds, d.Round(time.Millisecond),
				(d / rounds).Round(time.Nanosecond))
			return nil
		},
	})

	h.Add(harness.Case{
		Name:   "sched/affinity",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			n := 0
			err := onEachCPU(func(cpu int) error { n++; return nil })
			if err != nil {
				return err
			}
			t.Logf("ran on %d CPUs", n)
			if n == 0 {
				return fmt.Errorf("could not pin to any CPU")
			}
			return nil
		},
	})
}

// onEachCPU pins the calling OS thread to each online CPU in turn and
// runs fn there.  The thread is deliberately never unlocked on the error
// path: it is left to die with its affinity mask, rather than handing a
// pinned thread back to the runtime.
func onEachCPU(fn func(cpu int) error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var orig cpuSet
	if err := schedGetaffinity(0, &orig); err != nil {
		return fmt.Errorf("sched_getaffinity: %w", err)
	}
	defer schedSetaffinity(0, &orig)

	for cpu := 0; cpu < 1024; cpu++ {
		if !orig.isSet(cpu) {
			continue
		}
		var one cpuSet
		one.set(cpu)
		if err := schedSetaffinity(0, &one); err != nil {
			return fmt.Errorf("pinning to cpu %d: %w", cpu, err)
		}
		if err := fn(cpu); err != nil {
			return err
		}
	}
	return nil
}

type cpuSet [16]uint64 // 1024 CPUs, the same shape glibc uses

func (s *cpuSet) set(cpu int)        { s[cpu/64] |= 1 << (uint(cpu) % 64) }
func (s *cpuSet) isSet(cpu int) bool { return s[cpu/64]&(1<<(uint(cpu)%64)) != 0 }

func schedGetaffinity(pid int, s *cpuSet) error {
	_, _, e := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY,
		uintptr(pid), unsafe.Sizeof(*s), uintptr(unsafe.Pointer(s)))
	if e != 0 {
		return e
	}
	return nil
}

func schedSetaffinity(pid int, s *cpuSet) error {
	_, _, e := syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY,
		uintptr(pid), unsafe.Sizeof(*s), uintptr(unsafe.Pointer(s)))
	if e != 0 {
		return e
	}
	return nil
}
