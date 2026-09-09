package tests

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

func registerEntry(h *harness.Harness) {
	// Single-stepping is the one guest-visible feature PVM implements
	// with a dedicated inhibitor bit in the switcher: while
	// SWITCH_FLAGS_SINGLE_STEP is set the direct guest-user switch is
	// disabled and every step goes the long way round.  If that bit is
	// mishandled the tracee either runs away or never moves, and this
	// test is what notices.
	h.Add(harness.Case{
		Name:    "entry/ptrace-singlestep",
		Suites:  []string{harness.Core},
		Timeout: 60 * 1e9,
		Fn: func(t *harness.T) error {
			// ptrace is per-thread: the tracer has to stay put.
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			self, err := os.Executable()
			if err != nil {
				self = "/init"
			}
			cmd := exec.Command(self, "victim-loop")
			cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
			cmd.Stdout, cmd.Stderr = nil, nil
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("starting the tracee: %w", err)
			}
			pid := cmd.Process.Pid
			defer func() {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				_, _ = syscall.Wait4(pid, nil, 0, nil)
			}()

			// The child stops at exec with SIGTRAP.
			var ws syscall.WaitStatus
			if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
				return fmt.Errorf("waiting for the initial stop: %w", err)
			}
			if !ws.Stopped() {
				return fmt.Errorf("the tracee did not stop at exec: %v", ws)
			}

			var first, last syscall.PtraceRegs
			if err := syscall.PtraceGetRegs(pid, &first); err != nil {
				return fmt.Errorf("PTRACE_GETREGS: %w", err)
			}

			const steps = 2000
			moved := 0
			last = first
			for i := 0; i < steps; i++ {
				if err := syscall.PtraceSingleStep(pid); err != nil {
					return fmt.Errorf("PTRACE_SINGLESTEP at step %d: %w", i, err)
				}
				if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
					return fmt.Errorf("wait at step %d: %w", i, err)
				}
				if ws.Exited() {
					return fmt.Errorf("the tracee exited after %d steps", i)
				}
				if !ws.Stopped() || ws.StopSignal() != syscall.SIGTRAP {
					return fmt.Errorf("step %d stopped with %v, want SIGTRAP", i, ws)
				}
				var regs syscall.PtraceRegs
				if err := syscall.PtraceGetRegs(pid, &regs); err != nil {
					return fmt.Errorf("PTRACE_GETREGS at step %d: %w", i, err)
				}
				if regs.Rip != last.Rip {
					moved++
				}
				last = regs
			}

			t.Logf("%d single steps, rip changed on %d of them", steps, moved)
			t.Logf("rip: start %#x, end %#x", first.Rip, last.Rip)
			if moved == 0 {
				return fmt.Errorf("rip never moved in %d single steps: "+
					"the tracee is not making progress", steps)
			}
			return nil
		},
	})

	// PTRACE_ATTACH to a running process, read its registers, detach.
	// Under PVM the tracee's register state lives in the PVCS, so a
	// stale copy shows up as nonsense here.
	h.Add(harness.Case{
		Name:   "entry/ptrace-attach",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			self, err := os.Executable()
			if err != nil {
				self = "/init"
			}
			cmd := exec.Command(self, "victim-loop")
			if err := cmd.Start(); err != nil {
				return err
			}
			pid := cmd.Process.Pid
			defer func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}()

			if err := syscall.PtraceAttach(pid); err != nil {
				return fmt.Errorf("PTRACE_ATTACH: %w", err)
			}
			var ws syscall.WaitStatus
			if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
				return fmt.Errorf("wait after attach: %w", err)
			}
			var regs syscall.PtraceRegs
			if err := syscall.PtraceGetRegs(pid, &regs); err != nil {
				return fmt.Errorf("PTRACE_GETREGS: %w", err)
			}
			t.Logf("tracee rip=%#x rsp=%#x cs=%#x ss=%#x", regs.Rip, regs.Rsp, regs.Cs, regs.Ss)

			// The tracee is user space, so CPL must be 3.  Under PVM the
			// guest runs at hardware CPL3 throughout and the virtual CS
			// is what has to say 3 here; a kernel CS would mean the
			// switcher handed back the wrong frame.
			if regs.Cs&3 != 3 {
				return fmt.Errorf("tracee CS is %#x: RPL %d, want 3", regs.Cs, regs.Cs&3)
			}
			if regs.Rip == 0 || regs.Rsp == 0 {
				return fmt.Errorf("tracee registers look empty: rip=%#x rsp=%#x",
					regs.Rip, regs.Rsp)
			}
			return syscall.PtraceDetach(pid)
		},
	})
}
