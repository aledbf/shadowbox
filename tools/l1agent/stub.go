package main

import (
	"os"
	"syscall"
)

// The agent binary on the payload share, which the stub hands over to.
const agentPath = payload + "/l1agent"

// runStub is what the L1 image runs at boot (the bash stub it replaced).
//
// Baked into the L1 image and never changed.  The real agent arrives on
// the 9p payload share, so iterating on it needs no image surgery: the
// first version of this project edited the image with debugfs instead and
// left the filesystem inconsistent.
//
// Unlike the agent it does not redirect to /dev/console: the unit's
// StandardOutput=journal+console carries these lines, as it did for the
// bash stub.
func runStub() int {
	echo("L1-STUB: mounting the payload share")
	if err := os.MkdirAll(payload, 0o777); err != nil {
		shErr("mkdir: cannot create directory '" + payload + "': " + errText(err))
	}
	if run([]string{"mount", "-t", "9p", "-o", "trans=virtio,version=9p2000.L", "payload", payload},
		os.Stdin, os.Stdout, os.Stderr) != 0 {
		echo("L1-STUB: could not mount the payload share")
		echo("PVMTEST-RESULT: fail stage=1 reason=no-payload")
		poweroff()
	}
	// As in the bash, a poweroff that returns falls through to the next
	// check rather than stopping here.
	if !executable(agentPath) {
		echo("L1-STUB: no agent on the share")
		echo("PVMTEST-RESULT: fail stage=1 reason=no-agent")
		poweroff()
	}
	err := syscall.Exec(agentPath, []string{agentPath}, os.Environ())
	// bash's exec of a file it cannot run ends a non-interactive shell with
	// 126 (127 when it is not there).
	msg, rc := startFailure(agentPath, err)
	shErr(msg)
	return rc
}
