# PVM: what it is, what it costs, what is left

For someone - or something - picking this up cold, with one of two jobs:

- **make it faster**, or
- **make it smaller**, meaning fewer changes to code shared with kernels that
  will never run a PVM guest.

Read "The design" and "Ruled out" before proposing anything.

Two repositories:

- `github.com/aledbf/linux`, branch **`pvm`** - the kernel series on upstream
  `master` (v7.3-rc2+27): 56 commits, each building host and guest on its own,
  plus one last commit marked NOT FOR UPSTREAM with the debug instrumentation
  (`CONFIG_KVM_PVM_STATS`).
- `pvm-testbed` - the harness, and only the harness: it carries no kernel.
  Kernels come from `KSRC`, or from a git tree and revision named by a
  battery's `kernel` directive. L0 runs L1 as an ordinary KVM guest, L1 loads
  `kvm-pvm` and runs the guest inside it. Everything below is measured in that
  nesting.

---

## The design

A PVM guest runs **at hardware CPL 3, in both of its own modes, sharing one
hardware CR3 with the host**. No VMX, no SVM, no EPT/NPT. That is why the thing
exists: a host needs no hardware virtualisation to run it.

Everything expensive follows from it:

- **Shadow paging, always.** There is no second-level translation. This is the
  dominant cost in every benchmark that touches page tables.
- **Two shadow roots per guest CR3**, one for each guest mode, because the
  guest kernel and guest user space are both CPL 3 and the only thing that can
  separate them is which root is loaded. So every guest ring switch is a CR3
  load.
- **Every leaf SPTE carries `USER`** (invariant M1), because the guest is CPL 3
  whichever mode it thinks it is in. Hardware SMEP, SMAP and LASS therefore
  cannot enforce the guest's own kernel/user split; SMEP is emulated with NX,
  SMAP has no substitute.
- **The host kernel's upper half is present in the table the guest runs on**,
  with `_PAGE_USER` stripped. That is what makes a CPL 3 guest possible at all.
- **The guest kernel lives in the lower half**, so the guest is a
  position-independent kernel (`CONFIG_X86_PIE`), and telling guest from host
  addresses is a sign test.

`Documentation/virt/kvm/x86/pvm-invariants.rst` in the kernel tree states the
properties this must hold and how each is checked, and `pvm-spec.rst` is the
guest-visible ABI. Read both before touching the switcher or the shadow MMU
hooks.

### The hardware floor

The host must be booted with `pvm_host`, which enables the switcher's entry
hooks (`X86_FEATURE_PVM_HOST`) only if the host has FSGSBASE, PCID and INVPCID,
no KPTI, no FRED and is not a Xen PV guest; `kvm-pvm` refuses to load without
it, and also on TDX/SEV-ES hosts. Without `pvm_host` the entry code is the
same as a kernel without the switcher. Hosts that need KPTI are CPUs that no
longer ship, and supporting them costs CR3 handling in shared entry code.
FRED replaces the IDT entry paths the switcher owns.

LA57 is supported but cannot be tested on this machine: the CPU has no LA57,
and QEMU's TCG does not implement PCID/INVPCID, so kvm-pvm cannot load under
TCG (`batteries/tcg.pvm` and `pkey-tcg.pvm` fail at module load).

### The switcher

`arch/x86/entry/entry_64_switcher.S` sits between the host's entry code and the
guest. It serves on its own, without an exit to KVM:

- **guest mode switches** (syscall, sysret, event return): a CR3 load between
  the two roots of the current guest CR3;
- **`PVM_HC_LOAD_PGTBL`**, a guest context switch: the hypervisor publishes up
  to four `(guest_cr3, smod_cr3, umod_cr3)` triples into `tss_ex.pgtbl` on every
  entry, and the switcher loads a pair it finds there. It must keep the mode
  direct switch alive (`SWITCH_FLAGS_NO_DS_CR3` cleared), or the exits it saves
  come back as `ERETU` exits;
- **direct #PF**, below.

---

## Performance

### Upstream KVM against PVM

`batteries/kvm-vs-pvm.pvm`: `master` (host and guest, `kvm-intel` in L1)
against `pvm` at 93a43289b682 (host and guest, `kvm-pvm`, direct #PF on), each a
timing build, same L1 shape, order alternating, medians of 5, on an idle
machine. i9-13900HK, L1 with 8 vCPUs and THP:

| benchmark | KVM, 1 cpu | PVM, 1 cpu | 1 cpu | 2 | 4 | 8 |
|---|---:|---:|---:|---:|---:|---:|
| `perf/syscall` (getpid) | 58.6 ns | 210 ns | 3.59x | 3.58x | 3.56x | 3.60x |
| `perf/context-switch` (pipe) | 5205 ns | 7504 ns | 1.44x | 1.57x | 1.72x | - |
| `perf/page-fault` | 997 ns | 2977 ns | 2.99x | 3.04x | 3.01x | 2.99x |
| `perf/parallel-fault` | 679 ns | 2769 ns | 4.08x | 3.20x | 7.84x | 5.38x |
| `perf/fault-scaling` | 786 ns | 2855 ns | 3.63x | 3.10x | 2.80x | 2.90x |
| `perf/fork-exec` | 457 µs | 1631 µs | 3.57x | 4.34x | 4.37x | 5.41x |
| `perf/parallel-fork` | 433 µs | 1580 µs | 3.65x | 3.94x | 5.02x | 6.77x |

All 120 boots passed, and the ranges are disjoint everywhere except page-fault
at 2 cpus. Context-switch loses reps at 2 and 4 cpus and reports nothing at 8 -
read that row as indicative. `parallel-fault` above one cpu has the widest
spread of the suite (the same build has measured 855-1466 ns at 8 cpus); compare
kernels on it only with an interleaved `ab`. The `fault-rounds` refaults are
2.3-4.7x.

**The nesting inflates PVM's numbers.** Every `vcpu_enter_guest()` reads
`MSR_IA32_DEBUGCTLMSR`, which in L1 is an exit to L0 costing ~0.8-1 µs.
kvm-intel pays it too, but PVM enters far more often (every fixed #PF and
hypercall is a full entry), so on bare metal the fault ratios would be lower.
A per-CPU DEBUGCTL shadow is the upstream-shaped fix and would help both
vendors.

### What the syscall costs

Using the guest's own KPTI as a reference for what a per-syscall CR3 switch
costs on this machine:

| guest | vs. KVM without guest KPTI |
|---|---|
| KVM, guest `pti=off` | 1.00x |
| KVM, guest `pti=on` | 2.6x |
| PVM | 3.6x |

**Most of PVM's syscall cost is the CR3 switch it cannot avoid.** A PVM guest
pays a KPTI-shaped syscall whether or not it enables KPTI, because the two-root
split is the only thing between guest user space and the guest kernel. PVM's
own event handling is the rest, of which roughly 13 ns is the protection-key
swap.

### What a page fault costs

Shadow paging makes a first touch of a page two faults: a not-present fault
the guest must see (its own PTE is empty), and a shadow fault once the guest
has filled it.

- **Direct #PF** (`PVM_FEATURE_DIRECT_PF`) serves the first without an exit.
  The switcher delivers a user-mode not-present fault straight to the guest's
  event entry, with no shadow walk. When only the shadow entry was missing the
  delivery is spurious - the guest finds its PTE present and returns, and the
  next fault on that page exits normally: never twice in a row on the same page,
  never more than 4 in a row. Spurious deliveries stay under 0.25%. The result
  is 1.0 exits per first-touched page instead of 2.0, and a third less time in
  page-fault and parallel-fault. It is negotiated: a CPUID bit, a guest opt-in
  in `MSR_PVM_FEATURES_ENABLED`, `pvm_direct_pf=off` on the guest command line
  to decline. `PVCS::cr2` is authoritative and synced into KVM after every run.
- **The shadow fault** that remains costs 1.5-4.8 µs. Timed under
  `CONFIG_KVM_PVM_STATS`: after the shadow MMU is done, re-entry is 0.9-1.8 µs,
  dominated by the nested DEBUGCTL read above.

---

## Ruled out, with reasons

Do not propose these without new evidence.

**Protection keys instead of the two-root split.** One shadow root for both
guest modes, with PKRU denying the kernel's keys in guest user mode. `WRPKRU`
is unprivileged and does not trap, so guest user code simply unlocks the
kernel's keys.

**PKS for the same purpose.** PKS governs supervisor-mode accesses to `U=0`
pages. M1 makes every page `U=1`. Nothing to govern.

**Sharing non-leaf shadow pages between the guest's two roots.** Fuses the
trees that `role.access & ACC_USER_MASK` separates and defeats the NX-based
SMEP emulation (M4, M3).

**Tuning `tlb_single_page_flush_ceiling` in the guest.** No effect on
`parallel-fault` from 1 to 33; exit counts unchanged.

**`HC_TLB_INVLPG` range batching.** Most single-page flushes happen in the
first 60 ms of guest boot, and benchmark ranges average 1.1 pages.

**x2APIC ICR / TSC_DEADLINE fast re-entry.** Makes context-switch slower
through more HLT cycles.

**Re-reading the guest PTE through a cached hva.** No measurable effect.

**A not-present SPTE marker** to skip the guest-visible fault. The marker is
always stale when the guest fills its PTE through an unsync page, and kept only
in synced pages it is hit ≤0.02%. Direct #PF is the ABI-level answer.

**Prefaulting the shadow entry at direct-#PF time, and a swapgs-only GSBASE
path.** Both need state the switcher does not have without a walk or an exit.

**Fast re-entry for the shadow fault.** The cost is in `vcpu_enter_guest()`
itself (the nested DEBUGCTL read). A PVM-private re-entry would duplicate its
request, event and FPU handling, or run MMU faults with IRQs off. Fix DEBUGCTL
instead.

**A negotiated linear-address range.** The guest owns the lower half; that is a
sign test, needs no MSR and is compatible with `CONFIG_KASAN_VMALLOC`.

**PV MMU as an obvious win.** KVM removed its own PV MMU because it was slower
than shadow paging. Not a closed door for PVM, which has no EPT to fall back
on, but read why it failed there first.

---

## Open leads: performance

1. **`fork-exec` and `parallel-fork`, 3.6-6.5x.** The largest gap and barely
   decomposed: shadow page churn on exec and exit, write-protection of the
   parent's tables on fork, `mmu_lock` contention (~400 ns/page waited at 8
   vCPUs in parallel-fault).
2. **DEBUGCTL on every entry**, in common KVM code. See above.
3. **The syscall's ~1.4x over a KPTI syscall.** The switcher does ~6 PVCS
   stores, a `swapgs` pair, `rdgsbase`/`wrgsbase`, and the guest then runs its
   own entry sequence.
4. **The protection-key swap, ~13 ns.** Unconditional for any guest with
   `CR4.PKE`; Linux tasks run on `init_pkru` = 0x55555554, so a "user PKRU
   equals supervisor" short circuit never fires. Making it free means letting
   supervisor mode run on the user's PKRU - a decision, not an optimisation.
5. **`XCR0` switching.** PVM keeps the guest's `XCR0` equal to the host's to
   avoid an `XSETBV` exit to L0 on every switch; the size of that cost has never
   been separated from run-to-run drift.

For anything else: `make exits`, then `pvmtest resolve-exits`, and look at the
top reasons with their guest RIPs. Per-case counters come from a `stats=on`
boot and `pvmtest stats <log>`.

---

## Open leads: fewer changes

`master..pvm` without the instrumentation commit: 56 commits, 118 files, 11373
insertions, 369 deletions, including two KVM selftests for PVM. The first five
commits are fixes that stand on
their own (objtool pv_ops matching, RDPKRU/WRPKRU emulation and its selftest,
exception state ordering, vendor-narrowed ARCH_CAPABILITIES).

| area | files | + | − | note |
|---|---:|---:|---:|---|
| `arch/x86/kvm/pvm` | 4 | 4298 | 0 | new, nothing shared |
| `Documentation` | 4 | 1251 | 0 | spec, invariants |
| `arch/x86/entry` | 13 | 1222 | 107 | switcher (new), guest entry, hooks |
| `arch/x86/include` | 26 | 1041 | 61 | |
| `arch/x86/kernel` | 19 | 991 | 47 | mostly PIE + the guest side |
| `tools` | 10 | 1472 | 15 | objtool, perf, three selftests |
| `arch/x86/kvm/mmu` | 4 | 258 | 29 | the shared shadow MMU |
| `arch/x86/boot` | 3 | 248 | 4 | early relocation, kernel mapping |
| `arch/x86/kvm` (other) | 13 | 176 | 28 | vendor hooks |
| `arch/x86/mm` | 8 | 101 | 19 | |
| the rest | 14 | 315 | 59 | Kconfig, Makefiles, relocs, bpf, power, xen |

Targets, in the order a reviewer would care:

1. **The PIE kernel is the biggest shared chunk.** Ask whether all of
   `CONFIG_X86_PIE` is needed, or whether a smaller "relocatable to the lower
   half" change would do. It is independent of PVM and arguably wants its own
   upstream life. The `xen/`, `power/`, `platform/pvh/` and `bpf` changes are
   PIE consequences, not PVM ones.
2. **The shared shadow MMU**, the part upstream will scrutinise hardest: one
   global (`shadow_guest_cpl3`) plus the host root, the protection key in the
   SPTE with `role.cr4_pke`, and `mmu_adjust_kernel_only_access()`.
3. **The entry hooks**: `ALTERNATIVE`s on `X86_FEATURE_PVM_HOST` in
   `entry_64.S`, `calling.h` and `asm_exc_page_fault`, patched in only on hosts
   booted with `pvm_host`.

---

## What is missing or open functionally

- **PKS, and therefore SMAP for the guest.** Supervisor mode runs on `PKRU` 0
  rather than on the guest's `MSR_IA32_PKRS`. See "Protection Keys" in
  `pvm-spec.rst`.
- **`set_memory_region_test` fails under PVM** and passes under kvm-intel:
  PVM does not produce `KVM_EXIT_INTERNAL_ERROR` for MMIO during event
  vectoring. Recorded in `configs/kvm-selftests-expect.txt` with the other
  per-vendor outcomes.
- **`security/pkey-allowed` fails intermittently under TCG** (4-level and LA57,
  direct #PF on or off) and never under KVM (0 of 200). Believed to be TCG;
  `batteries/pkey-tcg.pvm` repeats it with PKRU printed on each failure.

---

## How to measure without fooling yourself

- **Run things through a battery** (`batteries/*.pvm`, `make battery B=...`),
  not a loop written for the occasion. It checks the host build an item needs,
  keeps every log with a manifest under `out/results/`, applies the
  known-failure lists, and reduces matrices and A/B runs the same way every time.
- **Timing needs a timing build.** `make host-kernel STATS=1` builds the
  counting host; the counters and stamps cost real time. Batteries with
  `host=timing` refuse the wrong one; `kernel` directives build their own image
  sets under `out/refs/`.
- **Never rebuild while a measurement runs.** Each boot stages the images it
  finds at that moment.
- **Correctness runs may be parallel (`-j`), timing runs never are.**
- **`pvmtest boot l1` does not build anything.** It boots whatever is in `out/`.
- **Do not check a kernel build with `grep error:`.** `modpost` failures read
  `ERROR: modpost: ...`. Check the exit status.
- **Metric lines must end with `#END`.** Interleaved `printk` can corrupt a
  metric line into a plausible wrong ratio; the parser ignores lines without it.
- **Single runs drift by tens of percent.** Medians of 5, and look at whether
  the ranges overlap.
- **Whole-suite exit histograms include guest boot.** Use per-case marks.
- **L1 has 8 vCPUs**: 16 guest vCPUs are overcommitted, not a scaling point.
  L1 needs THP; without it kvm-intel's page-fault is 9 µs and PVM looks faster.
- **`pr_info` from the module does not reach L1's live console during a guest
  run; `pr_emerg` does.**
- **`L1_QMP=<path>`** opens a QMP socket on L1's QEMU; `info registers -a`
  shows where a wedged L1's CPUs are.
- **`SELFTESTS=<glob>`** stages only the KVM selftests you are asking about.
- **`pvmtest.guest_timeout=<s>`** shortens the wait for a hanging guest.
- **Under TCG**, INVD/WBINVD do not fault at CPL 3 and `time/monotonic` times
  out. Emulator artefacts; nothing measured under TCG means anything.

## The ABI

`PVM_CPUID_SIGNATURE` / `PVM_CPUID_FEATURES` (0x40000200) carry an ABI version
and a feature bitmap, and the guest refuses a version it was not built for.
Optional behaviour takes a `PVM_FEATURE_` bit, which the guest enables in
`MSR_PVM_FEATURES_ENABLED` (per vCPU, 0 at reset, #GP on unknown bits), and
needs no version bump. `PVM_FEATURE_DIRECT_PF` (bit 0) is the one defined.
