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
	"unsafe"
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

	// Never hang: a victim that neither dies nor finishes tells the
	// harness nothing, and its output is only read once it exits.
	itv := itimerval{Value: timeval{Sec: 5}}
	syscall.Syscall(sysSetitimer, itimerReal, uintptr(unsafe.Pointer(&itv)), 0)
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

// victimPkey exercises a memory protection key from an unprivileged
// process: allocate one, put it on a page, and touch the page.
//
// On PVM this is the whole protection-key path end to end.  The key comes
// from the guest's own page table entry, which the shadow MMU has to copy
// into the leaf SPTE (shadow_pkey_mask), and the PKRU that decides what the
// key means is the real hardware register, which the guest kernel wrote and
// which has to survive the ring switch back to user mode.  Any one of those
// missing and the access is simply allowed.
//
// "deny" allocates the key with PKEY_DISABLE_ACCESS and expects to die.
// "allow" allocates it with no restriction and expects to live: without
// that control, a page that faulted for some unrelated reason would make
// the first case pass for the wrong reason.
func victimPkey(mode string) {
	// PKRU is per-thread and pkey_alloc sets it for the calling thread
	// only, so the access has to happen on that same thread.
	runtime.LockOSThread()

	// Never hang: a victim that neither dies nor finishes tells the
	// harness nothing, and its output is only read once it exits.
	itv := itimerval{Value: timeval{Sec: 5}}
	syscall.Syscall(sysSetitimer, itimerReal, uintptr(unsafe.Pointer(&itv)), 0)

	rights := uintptr(0)
	if mode == "deny" {
		rights = pkeyDisableAccess
	}
	// Worth printing: a kernel that did not enable protection keys would
	// make pkey_alloc fail rather than make this case pass quietly, but
	// when it does fail this says why in one line.
	cpu, _ := os.ReadFile("/proc/cpuinfo")
	fmt.Fprintf(os.Stderr, "victim: kernel sees ospke=%v pku=%v\n",
		bytes.Contains(cpu, []byte(" ospke")), bytes.Contains(cpu, []byte(" pku")))

	key, _, errno := syscall.Syscall(sysPkeyAlloc, 0, rights, 0)
	if errno != 0 {
		// No protection keys in this guest: report it as a skip rather
		// than a failure, and let the harness decide.
		fmt.Fprintf(os.Stderr, "victim: pkey_alloc: %v\n", errno)
		os.Exit(77)
	}
	fmt.Fprintf(os.Stderr, "victim: pkey_alloc -> %d\n", key)

	b, err := syscall.Mmap(-1, 0, os.Getpagesize(),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "victim: mmap: %v\n", err)
		os.Exit(3)
	}
	b[0] = 0x5a

	_, _, errno = syscall.Syscall6(sysPkeyMprotect,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		syscall.PROT_READ|syscall.PROT_WRITE, key, 0, 0)
	if errno != 0 {
		fmt.Fprintf(os.Stderr, "victim: pkey_mprotect: %v\n", errno)
		os.Exit(3)
	}

	fmt.Printf("victim: reached (key %d, %s)\n", key, mode)
	os.Stdout.Sync()

	if b[0] != 0x5a {
		fmt.Fprintf(os.Stderr, "victim: read back %#x\n", b[0])
		os.Exit(4)
	}
	fmt.Printf("victim: read through key %d succeeded\n", key)
	os.Exit(0)
}

