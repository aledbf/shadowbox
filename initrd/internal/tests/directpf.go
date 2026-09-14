package tests

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

// The cases where PVM_FEATURE_DIRECT_PF delivers a fault speculatively: the
// hardware says "not present" for a guest PTE that is present but has no
// shadow translation yet.  Each asserts what the guest must end up seeing --
// the right data, or the right signal with the right si_code -- which is the
// same with the feature on or off; a counting host shows which path ran.
func registerDirectPF(h *harness.Harness) {
	// A forked child's first touch of every inherited page is a present
	// guest PTE in an address space the hypervisor has never shadowed:
	// exactly the ambiguous fault, 64 times for reads and 64 more for the
	// copy-on-write writes after them.
	h.Add(harness.Case{
		Name:   "mm/direct-pf/fork-present-and-cow",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			rc, out, err := runVictim("victim-forktouch")
			if err != nil {
				return err
			}
			if rc != 0 {
				return fmt.Errorf("rc=%d: %s", rc, trim(out))
			}
			t.Logf("%s", trim(out))
			return nil
		},
	})

	// Write to a present, read-only page: whether it arrives as a
	// speculative P=0 or as the exact protection fault, the task must die of
	// SIGSEGV with SEGV_ACCERR, at that address, and the byte must not change.
	h.Add(harness.Case{
		Name:   "mm/direct-pf/readonly-write",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			rc, out, err := runVictim("victim-rowrite")
			if err != nil {
				return err
			}
			if sig := deathSignal(rc, out); sig != syscall.SIGSEGV {
				return fmt.Errorf("died of %v (rc=%d), want SIGSEGV: %s", sig, rc, trim(out))
			}
			if code := sigCode(out); code != 2 {
				return fmt.Errorf("si_code %d, want SEGV_ACCERR (2): %s", code, trim(out))
			}
			return nil
		},
	})

	// A read through a key that denies access: SEGV_PKUERR, never a plain
	// not-present fault the guest would retry, and never a successful read.
	h.Add(harness.Case{
		Name:   "mm/direct-pf/pkey-denied-code",
		Suites: []string{harness.Security, harness.Full},
		Fn: func(t *harness.T) error {
			rc, out, err := runVictim("victim-pkey", "deny")
			if err != nil {
				return err
			}
			if rc == 77 {
				t.Logf("skipped: the guest kernel has no protection keys")
				return nil
			}
			if sig := deathSignal(rc, out); sig != syscall.SIGSEGV {
				return fmt.Errorf("died of %v (rc=%d), want SIGSEGV: %s", sig, rc, trim(out))
			}
			if code := sigCode(out); code != 4 {
				return fmt.Errorf("si_code %d, want SEGV_PKUERR (4): %s", code, trim(out))
			}
			return nil
		},
	})

	// security/pkey-allowed repeated, for the intermittent failure under TCG
	// with LA57: how often, and with what PKRU.  Not in any suite; run it
	// by name.
	h.Add(harness.Case{
		Name:    "security/pkey-allowed-repeat",
		Suites:  []string{harness.Scaling},
		Timeout: 3600 * 1e9,
		Fn: func(t *harness.T) error {
			const runs = 200
			failed := 0
			for i := 0; i < runs; i++ {
				rc, out, err := runVictim("victim-pkey", "allow")
				if err != nil {
					return err
				}
				if rc == 77 {
					t.Logf("skipped: the guest kernel has no protection keys")
					return nil
				}
				if rc != 0 {
					failed++
					if failed <= 5 {
						t.Logf("run %d: rc=%d %s", i, rc, trim(out))
					}
				}
			}
			t.Metric("failed", float64(failed), "count")
			if failed > 0 {
				return fmt.Errorf("%d of %d allowed reads failed", failed, runs)
			}
			return nil
		},
	})

	// Permissions flipping under concurrent readers: a page goes read-only,
	// its SPTE is dropped or write-protected, it comes back writable and is
	// written, all while other CPUs fault on it and its neighbours in the
	// same page table.
	h.Add(harness.Case{
		Name:    "mm/direct-pf/mprotect-race",
		Suites:  []string{harness.Core},
		Timeout: 120 * 1e9,
		Fn:      mprotectRace,
	})
}

var sigCodeRe = regexp.MustCompile(`code=(0x[0-9a-f]+|[0-9]+)`)

// sigCode is the si_code from the Go runtime's crash report, or -1.
func sigCode(out []byte) int {
	m := sigCodeRe.FindSubmatch(out)
	if m == nil {
		return -1
	}
	var v int
	if _, err := fmt.Sscanf(string(m[1]), "%v", &v); err != nil {
		return -1
	}
	return v
}

// victimForkTouch forks with the raw system call and has the child touch
// every inherited page, read and then write, using nothing but memory
// accesses and raw system calls -- the Go runtime is not usable in a child
// of fork().  The child's exit status says which page went wrong; the parent
// then checks its own copy was not written through.
func victimForkTouch() {
	runtime.LockOSThread()
	debug.SetGCPercent(-1)

	const pages = 64
	page := os.Getpagesize()
	b, err := syscall.Mmap(-1, 0, pages*page,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "victim: mmap: %v\n", err)
		os.Exit(3)
	}
	base := unsafe.Pointer(&b[0])
	for i := 0; i < pages; i++ {
		for off := 0; off < page; off++ {
			*(*byte)(unsafe.Add(base, i*page+off)) = byte(0xa0 + i)
		}
	}

	pid, _, errno := syscall.RawSyscall(syscall.SYS_FORK, 0, 0, 0)
	if errno != 0 {
		fmt.Fprintf(os.Stderr, "victim: fork: %v\n", errno)
		os.Exit(3)
	}
	if pid == 0 {
		for i := 0; i < pages; i++ {
			if *(*byte)(unsafe.Add(base, i*page+100)) != byte(0xa0+i) {
				syscall.RawSyscall(syscall.SYS_EXIT_GROUP, uintptr(10+i), 0, 0)
			}
		}
		for i := 0; i < pages; i++ {
			*(*byte)(unsafe.Add(base, i*page+200)) = 0x55
			if *(*byte)(unsafe.Add(base, i*page+200)) != 0x55 ||
				*(*byte)(unsafe.Add(base, i*page+100)) != byte(0xa0+i) {
				syscall.RawSyscall(syscall.SYS_EXIT_GROUP, uintptr(80+i), 0, 0)
			}
		}
		syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)
	}

	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(int(pid), &ws, 0, nil); err != nil {
		fmt.Fprintf(os.Stderr, "victim: wait4: %v\n", err)
		os.Exit(3)
	}
	if !ws.Exited() || ws.ExitStatus() != 0 {
		st := ws.ExitStatus()
		switch {
		case ws.Signaled():
			fmt.Fprintf(os.Stderr, "victim: child killed by %v\n", ws.Signal())
		case st >= 80:
			fmt.Fprintf(os.Stderr, "victim: child's COW write to page %d did not stick\n", st-80)
		case st >= 10:
			fmt.Fprintf(os.Stderr, "victim: child read the wrong byte from page %d\n", st-10)
		default:
			fmt.Fprintf(os.Stderr, "victim: child exited %d\n", st)
		}
		os.Exit(4)
	}
	for i := 0; i < pages; i++ {
		if *(*byte)(unsafe.Add(base, i*page+200)) != byte(0xa0+i) {
			fmt.Fprintf(os.Stderr, "victim: the child's write reached the parent's page %d\n", i)
			os.Exit(5)
		}
	}
	fmt.Printf("child read %d inherited pages and copied them on write; parent intact\n", pages)
	os.Exit(0)
}

// victimRoWrite writes to a page it has made read-only after populating it.
func victimRoWrite() {
	page := os.Getpagesize()
	b, err := syscall.Mmap(-1, 0, page,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "victim: mmap: %v\n", err)
		os.Exit(3)
	}
	b[7] = 0x11
	if err := syscall.Mprotect(b, syscall.PROT_READ); err != nil {
		fmt.Fprintf(os.Stderr, "victim: mprotect: %v\n", err)
		os.Exit(3)
	}
	if b[7] != 0x11 {
		fmt.Fprintf(os.Stderr, "victim: read back %#x\n", b[7])
		os.Exit(4)
	}
	b[7] = 0x22 // must fault
	fmt.Fprintf(os.Stderr, "victim: the write to a read-only page returned (%#x)\n", b[7])
	os.Exit(0)
}

func mprotectRace(t *harness.T) error {
	const pages = 64
	const cycles = 1500
	page := syscall.Getpagesize()
	n := runtime.NumCPU()
	if n < 2 {
		n = 2
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(n))

	b, err := syscall.Mmap(-1, 0, pages*page,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		return err
	}
	defer syscall.Munmap(b)

	var stop atomic.Bool
	var reads atomic.Uint64
	errs := make([]error, n)
	var wg sync.WaitGroup
	for r := 1; r < n; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			runtime.LockOSThread()
			for !stop.Load() {
				for p := 0; p < pages; p++ {
					v := b[p*page+r%page]
					if v != 0 && v&1 == 0 {
						errs[r] = fmt.Errorf("reader %d: page %d holds %#x", r, p, v)
						stop.Store(true)
						return
					}
				}
				reads.Add(pages)
			}
		}(r)
	}

	start := time.Now()
	for c := 0; c < cycles && !stop.Load(); c++ {
		mark := byte(c<<1 | 1)
		if err := syscall.Mprotect(b, syscall.PROT_READ); err != nil {
			errs[0] = fmt.Errorf("mprotect ro: %w", err)
			break
		}
		if err := syscall.Mprotect(b, syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
			errs[0] = fmt.Errorf("mprotect rw: %w", err)
			break
		}
		for p := 0; p < pages; p++ {
			for i := 0; i < n; i++ {
				b[p*page+i%page] = mark
			}
		}
		if c%8 == 7 {
			if err := syscall.Madvise(b, syscall.MADV_DONTNEED); err != nil {
				errs[0] = fmt.Errorf("madvise: %w", err)
				break
			}
		}
	}
	stop.Store(true)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	t.Logf("%d protection cycles of %d pages, %d reads from %d CPUs in %s",
		cycles, pages, reads.Load(), n-1, time.Since(start).Round(time.Millisecond))
	return nil
}
