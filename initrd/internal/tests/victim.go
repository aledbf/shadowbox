package tests

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// runVictim re-executes this binary in one of the victim modes and
// returns the exit status and whatever it printed.  A non-zero status
// from a signal is reported as 128+signal, the shell convention, so a
// caller can tell "died" from "exited badly".
func runVictim(mode string, extra ...string) (int, []byte, error) {
	self, err := os.Executable()
	if err != nil {
		self = "/init"
	}
	cmd := exec.Command(self, append([]string{mode}, extra...)...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	if err == nil {
		return 0, buf.Bytes(), nil
	}
	var ee *exec.ExitError
	if !errorsAs(err, &ee) {
		return -1, buf.Bytes(), fmt.Errorf("running %s: %w", mode, err)
	}
	ws := ee.Sys().(syscall.WaitStatus)
	if ws.Signaled() {
		return 128 + int(ws.Signal()), buf.Bytes(), nil
	}
	return ws.ExitStatus(), buf.Bytes(), nil
}

// victimSegv writes through a PROT_NONE mapping.  It must not survive.
func victimSegv() {
	page := os.Getpagesize()
	b, err := syscall.Mmap(-1, 0, page,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "victim: mmap: %v\n", err)
		os.Exit(3)
	}
	if err := syscall.Mprotect(b, syscall.PROT_NONE); err != nil {
		fmt.Fprintf(os.Stderr, "victim: mprotect: %v\n", err)
		os.Exit(3)
	}
	b[0] = 1 // must fault
	fmt.Fprintln(os.Stderr, "victim: the write to a PROT_NONE page returned")
	os.Exit(0)
}

// victimLoop spins forever, doing a syscall each time round so that a
// tracer single-stepping it has something to step through.
func victimLoop() {
	for {
		syscall.Getpid()
	}
}
