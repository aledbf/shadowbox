package tests

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
	"unsafe"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

func registerSignal(h *harness.Harness) {
	// Signal delivery is an IRET back into user space with a rewritten
	// frame.  Under PVM that frame goes through the PVCS rather than the
	// hardware stack, so this is switcher territory.
	h.Add(harness.Case{
		Name:   "signal/delivery",
		Suites: []string{harness.Smoke},
		Fn: func(t *harness.T) error {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, syscall.SIGUSR1)
			defer signal.Stop(ch)

			if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
				return err
			}
			select {
			case s := <-ch:
				if s != syscall.SIGUSR1 {
					return fmt.Errorf("got %v, want SIGUSR1", s)
				}
			case <-time.After(5 * time.Second):
				return fmt.Errorf("SIGUSR1 was never delivered")
			}
			return nil
		},
	})

	// Many signals in a row: each one is an entry and a return, so this
	// is the cheapest way to put pressure on that path.
	h.Add(harness.Case{
		Name:   "signal/storm",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			const n = 20000
			ch := make(chan os.Signal, 64)
			signal.Notify(ch, syscall.SIGUSR2)
			defer signal.Stop(ch)

			var got int
			done := make(chan struct{})
			go func() {
				for range ch {
					got++
					if got >= n/2 {
						close(done)
						return
					}
				}
			}()
			for i := 0; i < n; i++ {
				_ = syscall.Kill(os.Getpid(), syscall.SIGUSR2)
			}
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				return fmt.Errorf("only %d of %d signals arrived", got, n)
			}
			t.Logf("delivered at least %d signals", got)
			return nil
		},
	})

	// Timers arrive as interrupts, which under PVM means the host has to
	// inject an event the guest picks up through the PVCS event flags.
	h.Add(harness.Case{
		Name:   "signal/timer",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, syscall.SIGALRM)
			defer signal.Stop(ch)

			start := time.Now()
			tv := itimerval{Value: timeval{Usec: 100000}} // 100ms, one shot
			if _, _, e := syscall.Syscall(sysSetitimer, itimerReal,
				uintptr(unsafe.Pointer(&tv)), 0); e != 0 {
				return fmt.Errorf("setitimer: %v", e)
			}
			select {
			case <-ch:
				t.Logf("SIGALRM after %s", time.Since(start).Round(time.Millisecond))
			case <-time.After(10 * time.Second):
				return fmt.Errorf("the interval timer never fired: interrupt injection is stuck")
			}
			return nil
		},
	})
}
