// Package tests holds the cases run inside the guest.
//
// They are ordinary Linux workloads, not PVM specific ones, and that is
// the point: PVM replaces the entry path, the page tables and the
// syscall return, so the way it breaks is that ordinary things stop
// working.  The few genuinely PVM specific checks live in pvm.go.
package tests

import (
	"fmt"
	"os"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

// Register adds every case.  Suite membership is on the case itself, so
// adding a test to a suite is a one word change next to the test.
func Register(h *harness.Harness) {
	registerBoot(h)
	registerPVM(h)
	registerMM(h)
	registerSyscall(h)
	registerSignal(h)
	registerSched(h)
	registerTime(h)
	registerEntry(h)
	registerStress(h)
	registerPerf(h)
}

// RunVictim is the entry point for the child processes some tests need:
// the same binary, re-executed with a mode argument.  A separate process
// is the only honest way to test a fault that kills whoever takes it.
func RunVictim(args []string) {
	switch args[0] {
	case "victim-segv":
		victimSegv()
	case "victim-loop":
		victimLoop()
	case "victim-exit":
		os.Exit(42)
	default:
		fmt.Fprintf(os.Stderr, "unknown victim mode: %s\n", args[0])
		os.Exit(127)
	}
}
