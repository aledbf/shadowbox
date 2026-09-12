package tests

// What the guest is told it has, and by whom.
//
// A PVM guest kernel reads CPUID through pv_ops.cpu.cpuid, which issues
// the synthetic "invlpg 0xffffffffff4d5650; cpuid" sequence the
// hypervisor emulates, so /proc/cpuinfo reflects what KVM chose to
// advertise.  A plain CPUID instruction at CPL3 is not trapped at all and
// answers from the hardware.  The two can therefore disagree, and this
// file is about finding out where.
//
// That matters beyond tidiness: any userspace library doing its own CPUID
// feature detection -- every optimised crypto and string library does --
// gets the host's answer, not the guest's.

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
	"github.com/aledbf/pvm-testbed/initrd/internal/sysinit"
)

//go:noescape
func cpuidRaw(leaf, sub uint32) (eax, ebx, ecx, edx uint32)

// A CPUID bit, named the way /proc/cpuinfo names it so the two can be
// compared without a translation table for the whole ISA.
type cpuidBit struct {
	flag  string // as it appears in /proc/cpuinfo
	leaf  uint32
	sub   uint32
	reg   int // 0=eax 1=ebx 2=ecx 3=edx
	bit   uint
}

// The features that carry an MSR or an architectural state PVM has to do
// something about.  Not the whole ISA: the point is the contract, not an
// inventory.
var watchedBits = []cpuidBit{
	// The SPEC_CTRL family.  Every one of these means "MSR_IA32_SPEC_CTRL
	// exists and accepts this bit".
	{"ibrs", 7, 0, 3, 26},        // SPEC_CTRL: IBRS/IBPB
	{"stibp", 7, 0, 3, 27},       // INTEL_STIBP
	{"ssbd", 7, 0, 3, 31},        // SPEC_CTRL_SSBD
	{"ibpb", 0x80000008, 0, 1, 12},
	{"ibrs", 0x80000008, 0, 1, 14}, // AMD_IBRS
	{"stibp", 0x80000008, 0, 1, 15},
	{"ssbd", 0x80000008, 0, 1, 24},
	{"virt_ssbd", 0x80000008, 0, 1, 25},

	// Protection keys.  Implemented: the guest's key reaches the leaf SPTE
	// and the hardware checks it against the guest's own PKRU.
	{"pku", 7, 0, 2, 3},
	{"ospke", 7, 0, 2, 4},

	// Supervisor-mode protections a CPL3 guest cannot get from hardware.
	{"smep", 7, 0, 1, 7},
	{"smap", 7, 0, 1, 20},

	// State PVM does not switch.
	{"xsaves", 13, 1, 0, 3},
	{"la57", 7, 0, 2, 16},
	{"pcid", 1, 0, 2, 17},
}

func sysfsLine(name string) string {
	b, err := os.ReadFile("/sys/devices/system/cpu/vulnerabilities/" + name)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return strings.TrimSpace(string(b))
}

// isPVMRun reports whether the harness was told this is meant to be a PVM
// guest.  The same image boots as an ordinary KVM guest in stage 0, where
// these features are legitimately present.
func isPVMRun() bool {
	return sysinit.Cmdline().Get("pvmtest.expect", "") == "pvm"
}

func cpuinfoFlags() (map[string]bool, error) {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return nil, err
	}
	flags := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) != "flags" {
			continue
		}
		for _, f := range strings.Fields(rest) {
			flags[f] = true
		}
		break
	}
	if len(flags) == 0 {
		return nil, fmt.Errorf("/proc/cpuinfo has no flags line")
	}
	return flags, nil
}

func (c cpuidBit) present() bool {
	// A leaf above the CPU's maximum reads as garbage rather than zero,
	// so check the bound first.
	base := uint32(0)
	if c.leaf >= 0x80000000 {
		base = 0x80000000
	}
	maxLeaf, _, _, _ := cpuidRaw(base, 0)
	if c.leaf > maxLeaf {
		return false
	}
	regs := make([]uint32, 4)
	regs[0], regs[1], regs[2], regs[3] = cpuidRaw(c.leaf, c.sub)
	return regs[c.reg]&(1<<c.bit) != 0
}

func registerCPUID(h *harness.Harness) {
	// Diagnostic, not an assertion: print where the kernel's view and
	// the hardware's disagree.  Under ordinary KVM they should not; under
	// PVM the untrapped CPUID makes them, and the size of the gap is the
	// thing worth looking at.
	h.Add(harness.Case{
		Name:   "pvm/cpuid-kernel-vs-hardware",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			flags, err := cpuinfoFlags()
			if err != nil {
				return err
			}
			var onlyHW, onlyKernel []string
			for _, c := range watchedBits {
				hw := c.present()
				kn := flags[c.flag]
				name := fmt.Sprintf("%s (%#x:%d bit %d)", c.flag, c.leaf, c.reg, c.bit)
				switch {
				case hw && !kn:
					onlyHW = append(onlyHW, name)
				case kn && !hw:
					onlyKernel = append(onlyKernel, name)
				}
			}
			sort.Strings(onlyHW)
			sort.Strings(onlyKernel)
			if len(onlyHW) == 0 && len(onlyKernel) == 0 {
				t.Logf("the kernel's CPUID and a userspace CPUID agree on all %d watched bits",
					len(watchedBits))
				return nil
			}
			for _, n := range onlyHW {
				t.Logf("hardware only (userspace sees it, the kernel does not): %s", n)
			}
			for _, n := range onlyKernel {
				t.Logf("kernel only (the kernel sees it, userspace does not): %s", n)
			}
			return nil
		},
	})

	// The contract: a feature whose MSR is not emulated must not be
	// advertised.  MSR_IA32_SPEC_CTRL is emulated nowhere -- not in
	// pvm_set_msr(), not in kvm_set_msr_common() -- and the guest reaches
	// it through PVM_HC_{RD,WR}MSR, which have no way to report that the
	// access went nowhere.  A guest that is told it has any of these
	// therefore reports a mitigation it does not have.
	//
	// ibpb is deliberately not in the list: MSR_IA32_PRED_CMD *is*
	// handled by common KVM, which issues the barrier for real.
	h.Add(harness.Case{
		Name:   "pvm/spec-ctrl-contract",
		Suites: []string{harness.Core},
		Fn: func(t *harness.T) error {
			flags, err := cpuinfoFlags()
			if err != nil {
				return err
			}
			var advertised []string
			for _, f := range []string{"ibrs", "stibp", "ssbd",
				"virt_ssbd", "amd_ssbd", "amd_ibrs"} {
				if flags[f] {
					advertised = append(advertised, f)
				}
			}
			spectreV2 := sysfsLine("spectre_v2")
			ssb := sysfsLine("spec_store_bypass")
			t.Logf("SPEC_CTRL family: %v", advertised)
			t.Logf("spectre_v2:        %s", spectreV2)
			t.Logf("spec_store_bypass: %s", ssb)

			if !isPVMRun() {
				t.Logf("not declared a PVM run; reported for comparison only")
				return nil
			}

			if len(advertised) != 0 {
				return fmt.Errorf("PVM advertises %v, but MSR_IA32_SPEC_CTRL "+
					"is not emulated: the guest will report a mitigation "+
					"it does not have", advertised)
			}

			// Protection keys are half-landed: the machinery under
			// them works (shadow_pkey_mask, and the guest's own
			// PKRU enforced by the hardware), but pvm_set_cpu_caps()
			// does not advertise PKU yet.  So assert consistency
			// rather than a fixed answer -- a kernel that sees PKU
			// without OSPKE, or the reverse, is broken either way --
			// and let security/pkey-denied say whether they work
			// wherever they are advertised.
			if flags["pku"] != flags["ospke"] {
				return fmt.Errorf("PVM advertises pku=%v but ospke=%v; "+
					"the guest cannot use one without the other",
					flags["pku"], flags["ospke"])
			}
			t.Logf("protection keys advertised to the guest kernel: %v", flags["pku"])

			// Invariant M4: the guest runs at CPL3 in both of its
			// modes, so hardware SMAP cannot separate them.  SMEP
			// has a substitute -- NX on the user mapping -- and is
			// advertised.  SMAP has none and must not be.
			if flags["smap"] {
				return fmt.Errorf("PVM advertises smap, but the guest runs " +
					"at CPL3 in both modes and nothing emulates " +
					"supervisor-mode access prevention (invariant M4)")
			}
			// eIBRS is selected from ARCH_CAPABILITIES rather than
			// CPUID, so clearing the CPUID bits is not enough on its
			// own -- kvm_caps.supported_arch_cap has to drop
			// ARCH_CAP_IBRS_ALL as well.  This is the check that
			// notices if it creeps back.
			//
			// Only the mode matters, which is the clause before the
			// first semicolon: the rest lists sub-mitigations and
			// includes negative statements like "PBRSB-eIBRS: Not
			// affected", which a naive substring match reads as an
			// IBRS mode.  It did, the first time this ran.
			mode, _, _ := strings.Cut(spectreV2, ";")
			if strings.Contains(mode, "IBRS") {
				return fmt.Errorf("spectre_v2 mode is %q: the guest believes "+
					"in an IBRS mode whose MSR writes go nowhere "+
					"(is ARCH_CAP_IBRS_ALL still being advertised?)",
					strings.TrimSpace(mode))
			}
			return nil
		},
	})
}
