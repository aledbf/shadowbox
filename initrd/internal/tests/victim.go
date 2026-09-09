package tests

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
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

// pipes is a plain unidirectional pipe with both ends kept, so the caller
// can hand one end to a child and close both later.
type pipes struct{ r, w *os.File }

func pipePair() (*pipes, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &pipes{r: r, w: w}, nil
}

func (p *pipes) close() {
	_ = p.r.Close()
	_ = p.w.Close()
}

// startVictimPinned re-executes this binary in a victim mode with the
// given files as fd 3, 4, ... and pins it to one CPU.
//
// The pinning is done by the child itself rather than here: taskset is not
// in the initrd, and setting affinity on the parent before fork would pin
// the parent too.
func startVictimPinned(mode string, extra []*os.File, cpu int) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		self = "/init"
	}
	cmd := exec.Command(self, mode, strconv.Itoa(cpu))
	cmd.ExtraFiles = extra
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", mode, err)
	}
	return cmd, nil
}

// victimPingPong echoes single bytes from fd 3 to fd 4 forever, pinned to
// the CPU named on the command line.  The parent pins itself to the same
// one, so every exchange costs a real context switch.
func victimPingPong(cpuArg string) {
	cpu, err := strconv.Atoi(cpuArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "victim: bad cpu %q\n", cpuArg)
		os.Exit(3)
	}
	runtime.LockOSThread()
	var set cpuSet
	set.set(cpu)
	if err := schedSetaffinity(0, &set); err != nil {
		fmt.Fprintf(os.Stderr, "victim: pinning to cpu %d: %v\n", cpu, err)
		os.Exit(3)
	}

	in := os.NewFile(3, "ping")
	out := os.NewFile(4, "pong")
	buf := []byte{0}
	for {
		if _, err := io.ReadFull(in, buf); err != nil {
			os.Exit(0) // the parent finished and closed the pipe
		}
		if _, err := out.Write(buf); err != nil {
			os.Exit(0)
		}
	}
}
