package tests

import (
	"fmt"
	"os"
	"strings"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
	"github.com/aledbf/pvm-testbed/initrd/internal/sysinit"
)

// startKernelMap is where an ordinary x86-64 kernel is linked and mapped.
// A PVM guest may not use the top 2GB at all, so it must not be here.
const startKernelMap = 0xffffffff80000000

func registerPVM(h *harness.Harness) {
	// The single most informative number in the whole suite.  It does not
	// assert, because the same image is expected to boot as an ordinary
	// KVM guest too, and there it should be at the usual address.
	h.Add(harness.Case{
		Name:   "pvm/kernel-map",
		Suites: []string{harness.Smoke},
		Fn: func(t *harness.T) error {
			base, err := sysinit.KernelTextBase()
			if err != nil {
				return err
			}
			if base == 0 {
				return fmt.Errorf("kallsyms gave 0 for _text: kptr_restrict is set, " +
					"or this is not running as root")
			}
			t.Logf("_text = 0x%016x", base)
			switch {
			case base >= startKernelMap:
				t.Logf("kernel is in the usual top-2GB mapping: not relocated")
			default:
				t.Logf("kernel was relocated out of the top 2GB: PVM early relocation ran")
			}
			return nil
		},
	})

	// When the harness says this is meant to be a PVM run, the
	// relocation is no longer optional.
	h.Add(harness.Case{
		Name:   "pvm/relocated",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			if sysinit.Cmdline().Get("pvmtest.expect", "") != "pvm" {
				t.Logf("not declared a PVM run (pvmtest.expect != pvm); nothing to check")
				return nil
			}
			base, err := sysinit.KernelTextBase()
			if err != nil {
				return err
			}
			if base >= startKernelMap {
				return fmt.Errorf("_text is at 0x%016x, still inside the top 2GB "+
					"the hypervisor withholds: the early relocation did not run", base)
			}
			t.Logf("_text = 0x%016x", base)
			return nil
		},
	})

	// __pa()/__va() have to agree with the relocated mapping.  If the
	// kernel moved but phys_base did not follow, physical addresses come
	// out shifted by the relocation delta, and iomem is where that shows
	// up without needing a module.
	h.Add(harness.Case{
		Name:   "pvm/phys-map",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			b, err := os.ReadFile("/proc/iomem")
			if err != nil {
				return err
			}
			var code, data bool
			for _, line := range strings.Split(string(b), "\n") {
				if !strings.Contains(line, "Kernel code") &&
					!strings.Contains(line, "Kernel data") {
					continue
				}
				lo, hi, ok := strings.Cut(strings.Fields(line)[0], "-")
				if !ok {
					return fmt.Errorf("unparsable iomem line: %q", line)
				}
				var a, z uint64
				fmt.Sscanf(lo, "%x", &a)
				fmt.Sscanf(hi, "%x", &z)
				// An all-zero range is what a restricted /proc/iomem
				// looks like, and it would pass a mere presence check
				// while saying nothing at all.
				if z == 0 {
					return fmt.Errorf("iomem range is zeroed -- not running as root, "+
						"or kptr_restrict is set: %q", strings.TrimSpace(line))
				}
				if z <= a {
					return fmt.Errorf("empty or inverted kernel range: %q", line)
				}
				t.Logf("iomem: %s", strings.TrimSpace(line))
				if strings.Contains(line, "Kernel code") {
					code = true
				} else {
					data = true
				}
			}
			if !code || !data {
				return fmt.Errorf("/proc/iomem has no Kernel code/data ranges: " +
					"__pa() of the kernel image is not resolving")
			}
			return nil
		},
	})

	h.Add(harness.Case{
		Name:   "pvm/paravirt",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			b, err := os.ReadFile("/proc/cpuinfo")
			if err != nil {
				return err
			}
			t.Logf("hypervisor flag: %v", strings.Contains(string(b), "hypervisor"))
			t.Logf("reported: %s", sysinit.Hypervisor())
			return nil
		},
	})
}
