package tests

// Negative tests: things a guest must not be able to do.
//
// These matter more under PVM than under VMX.  A PVM guest runs at
// hardware CPL3 in *both* of its modes, so the CPU is not enforcing the
// guest's own kernel/user split at all -- every shadow PTE the guest can
// reach is a user PTE (invariant M1), and the split is reconstructed by
// validate_pvm_indirect_access() plus forced NX in __link_shadow_page()
// (invariant M4).  If that reconstruction is wrong, an ordinary
// unprivileged process in the guest can read guest kernel memory, and
// nothing else in this suite would notice.  Hence security/kernel-text.
//
// Likewise, a privileged instruction issued from guest user mode has to
// trap to the *guest* kernel.  On this design there is no VM exit for it
// to take, so "it faulted" and "the host handled it" are the same
// hardware event distinguished only by the switcher's bookkeeping.
//
// What is NOT here, and why: fuzzing MSR_PVM_*, the PVCS pinning and the
// hypercall ABI all need guest CPL0.  Nothing an unprivileged process can
// do reaches them.  They belong in a host-side test that drives a vCPU
// through the KVM API directly -- see docs/STATUS.md.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
	"github.com/aledbf/pvm-testbed/initrd/internal/sysinit"
)

//go:noescape
func callAddr(fn uintptr)

//go:noescape
func readAddr(p uintptr) byte

// mapFixedNoreplace is not in Go's syscall package.
const mapFixedNoreplace = 0x100000

// firstNoncanonical is the lowest address with bit 47 set and the top
// bits clear, i.e. the bottom of the canonical hole on a 4-level machine.
// Touching it is a #GP, not a #PF, which is a different path through the
// switcher than an ordinary fault.
const firstNoncanonical = 0x0000800000000000

// userKernelBoundary is the first address this guest's user space may not
// use, which is not the same number on every kernel this binary runs on.
//
// An ordinary 4-level kernel keeps its own half above the sign bit and user
// space runs to 1<<47.  A PVM guest has no upper half -- the host owns it,
// which is what lets the guest run at hardware CPL3 inside it -- so the guest
// kernel shares the lower half with its user space and the boundary is one
// bit further down.  The same initrd boots both, so it has to ask: the
// hypervisor bit in /proc/cpuinfo is the question.
//
// A 5-level guest would have a higher boundary either way and this would then
// be conservative rather than wrong; the guest kernel here is 4-level.
func userKernelBoundary() (uintptr, error) {
	flags, err := cpuinfoFlags()
	if err != nil {
		return 0, err
	}
	if flags["pvm_guest"] {
		return 1 << 46, nil
	}
	return 1 << 47, nil
}

// topUserPage is the last page the guest can actually map: mmap wants the
// whole mapping to end at or below TASK_SIZE_MAX, which is one page below the
// boundary, so the highest page that can be placed starts one page below that
// again.  Getting this wrong is what the control in security/mmap-boundary
// caught the first time it ran.
func topUserPage(boundary uintptr) uintptr {
	return boundary - 2*4096
}

// privInsns are instructions that must fault at CPL3.  The bytes are
// followed by a RET so that a CPU which somehow executed them would
// return cleanly and the test would report "it returned" rather than
// wandering off.
var privInsns = map[string][]byte{
	"hlt":         {0xf4, 0xc3},
	"cli":         {0xfa, 0xc3},
	"sti":         {0xfb, 0xc3},
	"wbinvd":      {0x0f, 0x09, 0xc3},
	"rdmsr":       {0x31, 0xc9, 0x0f, 0x32, 0xc3},       // xor %ecx,%ecx; rdmsr
	"wrmsr":       {0x31, 0xc9, 0x0f, 0x30, 0xc3},       // xor %ecx,%ecx; wrmsr
	"swapgs":      {0x0f, 0x01, 0xf8, 0xc3},
	"mov-cr3-rax": {0x0f, 0x20, 0xd8, 0xc3},             // mov %cr3,%rax
	"mov-dr7-rax": {0x0f, 0x21, 0xf8, 0xc3},             // mov %dr7,%rax
	"invd":        {0x0f, 0x08, 0xc3},
	"rdpmc":       {0x31, 0xc9, 0x0f, 0x33, 0xc3},       // xor %ecx,%ecx; rdpmc
}

func registerSecurity(h *harness.Harness) {
	// Every privileged instruction, one victim process each.  Separate
	// processes because the point is that the instruction kills whoever
	// issues it.
	for name := range privInsns {
		name := name
		h.Add(harness.Case{
			Name:   "security/priv-insn/" + name,
			Suites: []string{harness.Security, harness.Full},
			Fn: func(t *harness.T) error {
				return expectDeath(t, "the instruction executed at CPL3",
					"victim-privinsn", name)
			},
		})
	}

	// NX, from the other direction: a page that is mapped but not
	// executable.  Under PVM every shadow PTE is a user PTE, so NX is
	// carrying weight it does not carry on bare metal.
	h.Add(harness.Case{
		Name:   "security/exec-noexec-page",
		Suites: []string{harness.Security, harness.Full},
		Fn: func(t *harness.T) error {
			return expectDeath(t, "a RET on a PROT_READ|PROT_WRITE page returned",
				"victim-execpage", "rw")
		},
	})

	// The control: the same call through a page that *is* executable has
	// to work.  Without it, a machine where callAddr() faulted for some
	// unrelated reason would make every test above pass.
	h.Add(harness.Case{
		Name:   "security/exec-exec-page",
		Suites: []string{harness.Security, harness.Full},
		Fn: func(t *harness.T) error {
			rc, out, err := runVictim("victim-execpage", "rwx")
			if err != nil {
				return err
			}
			if rc != 0 {
				return fmt.Errorf("a RET on a PROT_EXEC page did not return: "+
					"rc=%d out=%q -- every other case in this suite is "+
					"passing for the wrong reason", rc, trim(out))
			}
			return nil
		},
	})

	// The one that matters most.  Guest kernel text, read from an
	// unprivileged guest process.  On PVM this is not enforced by the
	// CPU's U/S bit -- the guest is at CPL3 either way -- but by the
	// shadow MMU reconstructing the split.
	h.Add(harness.Case{
		Name:   "security/kernel-text",
		Suites: []string{harness.Security, harness.Core, harness.Full},
		Fn: func(t *harness.T) error {
			base, err := sysinit.KernelTextBase()
			if err != nil {
				return err
			}
			if base == 0 {
				return fmt.Errorf("kallsyms gave 0 for _text: kptr_restrict is set")
			}
			t.Logf("reading _text = 0x%016x from an unprivileged process", base)
			return expectDeath(t,
				fmt.Sprintf("guest kernel text at 0x%016x is readable from user mode", base),
				"victim-readaddr", fmt.Sprintf("%#x", base))
		},
	})

	// The host's window.  A PVM guest is confined to the linear range
	// the hypervisor granted it; the host's own kernel mapping lives
	// above it in the same shadow root, which is precisely why it must
	// not be reachable.  0xffffffff81000000 is where an ordinary x86-64
	// kernel puts its text.
	h.Add(harness.Case{
		Name:   "security/host-window",
		Suites: []string{harness.Security, harness.Core, harness.Full},
		Fn: func(t *harness.T) error {
			const hostText = 0xffffffff81000000
			base, err := sysinit.KernelTextBase()
			if err == nil && base >= startKernelMap {
				t.Logf("this kernel is itself at 0x%016x -- not a relocated "+
					"PVM guest, so this is the same check as kernel-text", base)
			}
			return expectDeath(t,
				"the host's kernel mapping is readable from the guest",
				"victim-readaddr", fmt.Sprintf("%#x", uint64(hostText)))
		},
	})

	// Non-canonical: a #GP rather than a #PF, and a different path.
	h.Add(harness.Case{
		Name:   "security/noncanonical-read",
		Suites: []string{harness.Security, harness.Full},
		Fn: func(t *harness.T) error {
			return expectDeath(t, "a non-canonical read returned",
				"victim-readaddr", fmt.Sprintf("%#x", uint64(firstNoncanonical)))
		},
	})

	// mmap has to refuse the addresses the guest may not have, rather
	// than handing them over and letting the fault sort it out.
	h.Add(harness.Case{
		Name:   "security/mmap-boundary",
		Suites: []string{harness.Security, harness.Core, harness.Full},
		Fn: func(t *harness.T) error {
			page := os.Getpagesize()
			boundary, err := userKernelBoundary()
			if err != nil {
				return err
			}
			top := topUserPage(boundary)
			// The last page of the user half has to still work: a
			// test that only shows high addresses failing would pass
			// just as well on a kernel that refused everything.
			p, _, errno := syscall.Syscall6(syscall.SYS_MMAP, top,
				uintptr(page), syscall.PROT_READ|syscall.PROT_WRITE,
				syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS|
					syscall.MAP_FIXED|mapFixedNoreplace,
				^uintptr(0), 0)
			if errno != 0 {
				return fmt.Errorf("mmap at the top user page (%#x) failed with %v: "+
					"the user/kernel boundary is not where it should be",
					top, errno)
			}
			syscall.Syscall(syscall.SYS_MUNMAP, p, uintptr(page), 0)
			t.Logf("%-28s %#016x -> ok (control)", "top user page", top)

			for _, tc := range []struct {
				name string
				addr uintptr
			}{
				// The first page user space may not have.  On a
				// PVM guest that is the guest kernel's own half,
				// in the lower half and perfectly canonical, so
				// nothing but TASK_SIZE_MAX is refusing it.
				{"first page above user space", boundary},
				{"non-canonical", firstNoncanonical},
				{"kernel half", 0xffffffff81000000},
				{"one page into the hole", firstNoncanonical + uintptr(page)},
			} {
				p, _, errno = syscall.Syscall6(syscall.SYS_MMAP, tc.addr,
					uintptr(page),
					syscall.PROT_READ|syscall.PROT_WRITE,
					syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS|
						syscall.MAP_FIXED|mapFixedNoreplace,
					^uintptr(0), 0)
				if errno == 0 {
					syscall.Syscall(syscall.SYS_MUNMAP, p, uintptr(page), 0)
					return fmt.Errorf("mmap(MAP_FIXED) at %s (%#x) succeeded, "+
						"returning %#x", tc.name, tc.addr, p)
				}
				t.Logf("%-28s %#016x -> %v", tc.name, tc.addr, errno)
			}
			return nil
		},
	})

	// The other half of the boundary, and the half only software enforces.
	//
	// security/kernel-text is the hardware's answer: a user process that
	// dereferences a guest kernel address faults, because the shadow MMU
	// reconstructed the U/S split the CPU can no longer see.  This is the
	// software's answer, to a user process that asks the *kernel* to do
	// the access for it by handing a guest kernel address to a syscall.
	//
	// Nothing in the hardware refuses that one.  The guest kernel runs at
	// CPL3 like its own user space, and its pages are USER in the shadow
	// tree -- invariant M1 -- so the kernel really can read and write them
	// on anyone's behalf.  access_ok() is the only thing that says no, and
	// access_ok() is TASK_SIZE_MAX, which on a PVM guest is no longer the
	// sign bit but a bound the guest kernel sets for itself.
	h.Add(harness.Case{
		Name:   "security/uaccess-boundary",
		Suites: []string{harness.Security, harness.Core, harness.Full},
		Fn: func(t *harness.T) error {
			zero, err := os.Open("/dev/zero")
			if err != nil {
				return err
			}
			defer zero.Close()
			null, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			defer null.Close()

			boundary, err := userKernelBoundary()
			if err != nil {
				return err
			}

			// The control, for the same reason mmap-boundary has
			// one: both directions have to work at an address the
			// process really owns, or a kernel that answered EFAULT
			// to everything would pass this.
			buf := make([]byte, 8)
			ok := uintptr(unsafe.Pointer(&buf[0]))
			if _, _, errno := syscall.Syscall(syscall.SYS_READ,
				zero.Fd(), ok, 8); errno != 0 {
				return fmt.Errorf("read(/dev/zero) into a valid buffer gave %v", errno)
			}
			if _, _, errno := syscall.Syscall(syscall.SYS_WRITE,
				null.Fd(), ok, 8); errno != 0 {
				return fmt.Errorf("write(/dev/null) from a valid buffer gave %v", errno)
			}
			t.Logf("%-28s %#016x -> ok (control)", "a buffer we own", ok)

			targets := []struct {
				name string
				addr uintptr
			}{
				{"first page above user space", boundary},
			}
			// A live, mapped, in-use guest kernel address: if the
			// bound were wrong the kernel would not merely touch a
			// hole, it would read and write its own text.
			if base, err := sysinit.KernelTextBase(); err == nil && base != 0 {
				targets = append(targets, struct {
					name string
					addr uintptr
				}{"guest kernel _text", uintptr(base)})
			}

			for _, tc := range targets {
				// read(2) writes into the buffer, so the kernel
				// would be the one writing to its own memory.
				_, _, errno := syscall.Syscall(syscall.SYS_READ,
					zero.Fd(), tc.addr, 8)
				if errno != syscall.EFAULT {
					return fmt.Errorf("read(/dev/zero) into %s (%#x) gave %v, want EFAULT: "+
						"the kernel accepted a user pointer above TASK_SIZE_MAX",
						tc.name, tc.addr, errno)
				}
				// write(2) reads out of it, which is the direction
				// that would hand kernel memory to user space.
				_, _, errno = syscall.Syscall(syscall.SYS_WRITE,
					null.Fd(), tc.addr, 8)
				if errno != syscall.EFAULT {
					return fmt.Errorf("write(/dev/null) from %s (%#x) gave %v, want EFAULT: "+
						"the kernel read its own memory for an unprivileged process",
						tc.name, tc.addr, errno)
				}
				t.Logf("%-28s %#016x -> EFAULT both ways", tc.name, tc.addr)
			}
			return nil
		},
	})

	// MAP_FIXED_NOREPLACE must not silently clobber. This is not
	// PVM specific, but it is the check that says the guest's own
	// address space bookkeeping survived the shadow MMU.
	h.Add(harness.Case{
		Name:   "security/mmap-noreplace",
		Suites: []string{harness.Security, harness.Full},
		Fn: func(t *harness.T) error {
			page := os.Getpagesize()
			b, err := syscall.Mmap(-1, 0, page,
				syscall.PROT_READ|syscall.PROT_WRITE,
				syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
			if err != nil {
				return err
			}
			defer syscall.Munmap(b)
			at := uintptr(unsafe.Pointer(&b[0]))
			b[0] = 0x5a

			_, _, errno := syscall.Syscall6(syscall.SYS_MMAP, at, uintptr(page),
				syscall.PROT_READ|syscall.PROT_WRITE,
				syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS|mapFixedNoreplace,
				^uintptr(0), 0)
			if errno != syscall.EEXIST {
				return fmt.Errorf("MAP_FIXED_NOREPLACE over a live mapping gave %v, "+
					"want EEXIST", errno)
			}
			if b[0] != 0x5a {
				return fmt.Errorf("the mapping was replaced anyway: byte is %#x", b[0])
			}
			return nil
		},
	})

	// Hostile syscall arguments.  The switcher's syscall path is the one
	// PVM replaces wholesale, and it has to survive garbage without the
	// host noticing.
	h.Add(harness.Case{
		Name:   "security/syscall-garbage",
		Suites: []string{harness.Security, harness.Full},
		Fn: func(t *harness.T) error {
			// An out-of-range syscall number.
			r, _, errno := syscall.Syscall(^uintptr(0)>>1, 0, 0, 0)
			if errno != syscall.ENOSYS {
				return fmt.Errorf("syscall(LONG_MAX) gave r=%d errno=%v, want ENOSYS",
					r, errno)
			}
			// A non-canonical pointer where a buffer is expected.
			_, _, errno = syscall.Syscall(syscall.SYS_UNAME, firstNoncanonical, 0, 0)
			if errno != syscall.EFAULT {
				return fmt.Errorf("uname(non-canonical) gave %v, want EFAULT", errno)
			}
			// A kernel address where a buffer is expected.
			_, _, errno = syscall.Syscall(syscall.SYS_UNAME, 0xffffffff81000000, 0, 0)
			if errno != syscall.EFAULT {
				return fmt.Errorf("uname(kernel address) gave %v, want EFAULT", errno)
			}
			t.Logf("ENOSYS, EFAULT, EFAULT -- all three refused in the guest")
			return nil
		},
	})
}

// expectDeath runs a victim and requires that it did not come back
// cleanly.  Go turns a fault in its own code into a fatal error rather
// than a signal death, so both are accepted; what is not accepted is
// exit 0, which means the thing under test succeeded.
func expectDeath(t *harness.T, whatWentWrong string, mode string, args ...string) error {
	rc, out, err := runVictim(mode, args...)
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), "victim: reached") {
		return fmt.Errorf("%s never got as far as the thing under test: rc=%d out=%q",
			mode, rc, trim(out))
	}
	if rc == 0 {
		return fmt.Errorf("%s: %s", mode, whatWentWrong)
	}
	// Everything in this file is a #GP or a #PF, and Linux delivers both
	// as SIGSEGV.  Requiring the signal rather than just "it died" is
	// what makes the test able to notice the guest taking the wrong
	// exception -- a #GP arriving as SIGILL, say, or as a SIGBUS from a
	// misdirected fault.
	sig := deathSignal(rc, out)
	if sig != syscall.SIGSEGV {
		return fmt.Errorf("%s died of %v, want SIGSEGV: %s", mode, sig, trim(out))
	}
	t.Logf("SIGSEGV, as required (rc=%d)", rc)
	return nil
}

// deathSignal works out which signal killed a victim.  Go does not let
// its own SIGSEGV handler default: a fault in Go code becomes "fatal
// error: ..." and exit 2, with the signal named on the following line.
// So the exit status is checked first and the output second.
func deathSignal(rc int, out []byte) syscall.Signal {
	if rc > 128 {
		return syscall.Signal(rc - 128)
	}
	for _, line := range strings.Split(string(out), "\n") {
		i := strings.Index(line, "[signal SIG")
		if i < 0 {
			continue
		}
		name, _, _ := strings.Cut(line[i+len("[signal "):], ":")
		for s := syscall.Signal(1); s < 32; s++ {
			if strings.EqualFold(strings.TrimSpace(s.String()), name) ||
				signalName(s) == name {
				return s
			}
		}
	}
	return 0
}

// signalName gives the SIGxxx spelling; Signal.String() gives the prose
// one ("segmentation violation"), which is not what Go prints in the
// bracketed line.
func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGBUS:
		return "SIGBUS"
	case syscall.SIGILL:
		return "SIGILL"
	case syscall.SIGFPE:
		return "SIGFPE"
	case syscall.SIGTRAP:
		return "SIGTRAP"
	case syscall.SIGSYS:
		return "SIGSYS"
	}
	return ""
}

// --- victims ------------------------------------------------------------

// victimPrivInsn writes one privileged instruction into an executable
// page and calls it.
func victimPrivInsn(name string) {
	code, ok := privInsns[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "victim: unknown instruction %q\n", name)
		os.Exit(3)
	}
	b, err := syscall.Mmap(-1, 0, os.Getpagesize(),
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "victim: mmap: %v\n", err)
		os.Exit(3)
	}
	copy(b, code)
	fmt.Fprintf(os.Stderr, "victim: reached %s\n", name)
	os.Stderr.Sync()
	callAddr(uintptr(unsafe.Pointer(&b[0])))
	fmt.Fprintf(os.Stderr, "victim: %s returned\n", name)
	os.Exit(0)
}

// victimExecPage calls a lone RET on a page with the given protection.
func victimExecPage(prot string) {
	p := syscall.PROT_READ | syscall.PROT_WRITE
	if prot == "rwx" {
		p |= syscall.PROT_EXEC
	}
	b, err := syscall.Mmap(-1, 0, os.Getpagesize(), p,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "victim: mmap: %v\n", err)
		os.Exit(3)
	}
	b[0] = 0xc3 // ret
	fmt.Fprintf(os.Stderr, "victim: reached %s page\n", prot)
	os.Stderr.Sync()
	callAddr(uintptr(unsafe.Pointer(&b[0])))
	os.Exit(0)
}

// victimReadAddr reads one byte from an address given in hex.
func victimReadAddr(addr string) {
	v, err := strconv.ParseUint(strings.TrimPrefix(addr, "0x"), 16, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "victim: bad address %q: %v\n", addr, err)
		os.Exit(3)
	}
	fmt.Fprintf(os.Stderr, "victim: reached read of %#x\n", v)
	os.Stderr.Sync()
	b := readAddr(uintptr(v))
	fmt.Fprintf(os.Stderr, "victim: read %#x from %#x\n", b, v)
	os.Exit(0)
}
