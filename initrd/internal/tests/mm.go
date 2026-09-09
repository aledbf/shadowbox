package tests

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

func registerMM(h *harness.Harness) {
	// PVM does shadow paging, so every one of these is a hypervisor code
	// path, not just a guest one.
	h.Add(harness.Case{
		Name:   "mm/anon-mmap",
		Suites: []string{harness.Smoke},
		Fn: func(t *harness.T) error {
			const size = 64 << 20
			b, err := syscall.Mmap(-1, 0, size,
				syscall.PROT_READ|syscall.PROT_WRITE,
				syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
			if err != nil {
				return fmt.Errorf("mmap %d bytes: %w", size, err)
			}
			// Touch every page: this is the fault storm the shadow MMU
			// has to service.
			for i := 0; i < size; i += os.Getpagesize() {
				b[i] = byte(i)
			}
			for i := 0; i < size; i += os.Getpagesize() {
				if b[i] != byte(i) {
					return fmt.Errorf("page at +%#x read back %#x, wrote %#x",
						i, b[i], byte(i))
				}
			}
			return syscall.Munmap(b)
		},
	})

	h.Add(harness.Case{
		Name:   "mm/mprotect",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			page := os.Getpagesize()
			b, err := syscall.Mmap(-1, 0, 4*page,
				syscall.PROT_READ|syscall.PROT_WRITE,
				syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
			if err != nil {
				return err
			}
			defer syscall.Munmap(b)
			b[0] = 1
			if err := syscall.Mprotect(b[:page], syscall.PROT_READ); err != nil {
				return fmt.Errorf("mprotect ro: %w", err)
			}
			if b[0] != 1 {
				return fmt.Errorf("read-only page lost its contents")
			}
			if err := syscall.Mprotect(b[:page], syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
				return fmt.Errorf("mprotect rw: %w", err)
			}
			b[0] = 2
			if b[0] != 2 {
				return fmt.Errorf("page did not become writable again")
			}
			return nil
		},
	})

	// A write to a PROT_NONE page must actually fault.  If the shadow
	// MMU dropped the protection, this test is the only thing that
	// notices, and it notices by the child dying.
	h.Add(harness.Case{
		Name:   "mm/protection-faults",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			code, out, err := runVictim("victim-segv")
			if err != nil {
				return err
			}
			if code == 0 {
				return fmt.Errorf("writing to a PROT_NONE page did not fault " +
					"-- the page protection was not enforced")
			}
			t.Logf("child died as expected (exit %d)", code)
			if !bytes.Contains(out, []byte("SIGSEGV")) &&
				!bytes.Contains(out, []byte("signal")) {
				t.Logf("note: child output did not mention SIGSEGV: %q", trim(out))
			}
			return nil
		},
	})

	h.Add(harness.Case{
		Name:   "mm/fork-cow",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			// Go cannot fork() safely, so copy-on-write is exercised
			// through a real fork+exec instead: the child gets its own
			// copy of everything and the parent's memory must survive.
			page := os.Getpagesize()
			b, err := syscall.Mmap(-1, 0, page,
				syscall.PROT_READ|syscall.PROT_WRITE,
				syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
			if err != nil {
				return err
			}
			defer syscall.Munmap(b)
			for i := range b {
				b[i] = 0xa5
			}
			if _, _, err := runVictim("victim-exit"); err != nil {
				return err
			}
			for i := range b {
				if b[i] != 0xa5 {
					return fmt.Errorf("byte %d changed under us: %#x", i, b[i])
				}
			}
			return nil
		},
	})

	h.Add(harness.Case{
		Name:   "mm/maps-sane",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			b, err := os.ReadFile("/proc/self/maps")
			if err != nil {
				return err
			}
			var n int
			for _, line := range strings.Split(string(b), "\n") {
				if line == "" {
					continue
				}
				n++
				lo, hi, ok := strings.Cut(strings.Fields(line)[0], "-")
				if !ok {
					return fmt.Errorf("unparsable maps line: %q", line)
				}
				var a, z uint64
				fmt.Sscanf(lo, "%x", &a)
				fmt.Sscanf(hi, "%x", &z)
				if z <= a {
					return fmt.Errorf("empty or inverted mapping: %q", line)
				}
				// User space must stay under the canonical hole.  A PVM
				// guest's user range is narrower still, but the kernel
				// picks it, so only the outer bound is checked here.
				if z > 0x00007fffffffffff+1 {
					return fmt.Errorf("user mapping above the canonical split: %q", line)
				}
			}
			t.Logf("%d mappings", n)
			return nil
		},
	})

	h.Add(harness.Case{
		Name:    "mm/churn",
		Suites:  []string{harness.Full},
		Timeout: 120 * 1e9,
		Fn: func(t *harness.T) error {
			// Repeated map/touch/unmap: this is what makes the shadow
			// MMU rebuild page tables, and where a stale SPTE shows up.
			page := os.Getpagesize()
			for i := 0; i < 4000; i++ {
				b, err := syscall.Mmap(-1, 0, 32*page,
					syscall.PROT_READ|syscall.PROT_WRITE,
					syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
				if err != nil {
					return fmt.Errorf("iteration %d: mmap: %w", i, err)
				}
				for j := 0; j < 32*page; j += page {
					b[j] = byte(i)
				}
				for j := 0; j < 32*page; j += page {
					if b[j] != byte(i) {
						return fmt.Errorf("iteration %d: stale page at +%#x", i, j)
					}
				}
				if err := syscall.Munmap(b); err != nil {
					return fmt.Errorf("iteration %d: munmap: %w", i, err)
				}
			}
			return nil
		},
	})
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
