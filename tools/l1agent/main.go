// Command l1agent is the in-VM side of the PVM testbed, static Go (it replaced
// a bash agent and stub; comments that say "the bash" refer to them).
//
//	l1agent stub   what the L1 image runs (/opt/pvm/agent stub): mounts the
//	               payload share and execs /mnt/payload/l1agent
//	l1agent        the agent proper, run from the payload share
//
// Its output is a contract read by the host side (tools/pvmtest: the log
// parser, the sanitizer, the selftest check, the exit profile), so every line
// is the one the bash printed -- with one deliberate exception: "L1: insmod
// failed", which the bash never managed to print.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// progName stands in for "$0: line N" in the messages bash prints itself.
const progName = "l1agent"

func main() {
	inheritSigpipe()
	switch {
	case len(os.Args) == 1:
		os.Exit(runAgent())
	case len(os.Args) == 2 && os.Args[1] == "stub":
		os.Exit(runStub())
	default:
		fmt.Fprintf(os.Stderr, "usage: %s [stub]\n", os.Args[0])
		os.Exit(2)
	}
}

// inheritSigpipe keeps SIGPIPE ignored for the commands this runs when it
// was ignored for us.  systemd starts services that way (IgnoreSIGPIPE=yes)
// and bash passes an inherited SIG_IGN on to its children; the Go runtime
// installs its own handler instead, which exec resets to the default.
func inheritSigpipe() {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(b), "\n") {
		v, ok := strings.CutPrefix(l, "SigIgn:")
		if !ok {
			continue
		}
		mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		if err == nil && mask&(1<<(uint(syscall.SIGPIPE)-1)) != 0 {
			signal.Ignore(syscall.SIGPIPE)
		}
		return
	}
}
