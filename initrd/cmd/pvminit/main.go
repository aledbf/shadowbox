// Command pvminit is PID 1 in the guest under test.
//
// It is the whole of user space: there is no shell and no libc in the
// initrd, so anything it does not do itself does not happen.  That is
// deliberate.  A guest that prints the banner below has executed a real
// exec, a real page fault, and a real syscall return through whatever
// entry path the kernel was built with, and none of it was smoothed over
// by a rescue shell.
//
// It must never leave the machine running.  The harness on the outside
// reads the serial log and kills the VM on a timeout, but a timeout takes
// three minutes and says nothing about what went wrong, so every path
// through this program ends in a result line and a power off.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
	"github.com/aledbf/pvm-testbed/initrd/internal/sysinit"
	"github.com/aledbf/pvm-testbed/initrd/internal/tests"
)

func main() {
	// The victim modes are the same binary re-executed with an argument.
	// Keeping them in one binary keeps the initrd to a single file.
	if len(os.Args) > 1 {
		tests.RunVictim(os.Args[1:])
		return
	}

	if os.Getpid() == 1 {
		sysinit.Setup()
		defer sysinit.PowerOff()
	}

	cmdline := sysinit.Cmdline()
	suite := cmdline.Get("pvmtest.suite", "default")
	tag := cmdline.Get("pvmtest.tag", "guest")

	fmt.Printf("\nPVMINIT: up  tag=%s suite=%s pid=%d\n", tag, suite, os.Getpid())
	sysinit.Banner()

	h := harness.New(suite, tag)
	tests.Register(h)

	start := time.Now()
	h.Run()
	h.Report(time.Since(start))
}
