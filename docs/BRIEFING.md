# PVM: where it stands, what it costs, what is left

For someone — or something — picking this up cold, with one of two jobs:

- **make it faster**, or
- **make it smaller**, meaning fewer changes to code shared with kernels that
  will never run a PVM guest.

Read "The commitment" and "Ruled out" before proposing anything. Most of the
obvious ideas in both directions have already been tried and measured, and the
reasons they failed are the useful part of this document.

Two repositories:

- `linux-aledbf`, branch **`pvm-7.3-series`** — the kernel series on
  `v7.3-rc2`. All work lands there; a rejected experiment is reverted on it,
  not parked on a branch. (The older `pvm-*-experiment` branches predate that
  rule.) Each commit builds (`bzImage` + `modules`) on its own.
- `pvm-testbed` — the harness, and only the harness: it carries no kernel.
  Kernels come from `KSRC`, or from a git tree and revision named by a
  battery's `kernel` directive. L0 runs L1 as an ordinary KVM guest, L1 loads
  `kvm-pvm` and runs the guest inside it. Everything below is measured in that
  nesting.

---

## The commitment

A PVM guest runs **at hardware CPL 3, in both of its own modes, sharing one
hardware CR3 with the host**. No VMX, no SVM, no EPT/NPT. That is the whole
idea and it is not negotiable — it is why the thing exists.

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
  SMAP has no substitute yet.
- **The host kernel's upper half is present in the table the guest runs on**,
  with `_PAGE_USER` stripped. That is what makes a CPL 3 guest possible at all.

`Documentation/virt/kvm/x86/pvm-invariants.rst` in the kernel tree states the
properties this must hold and how each is checked, and `pvm-spec.rst` is the
guest-visible ABI. Read both before touching the switcher or the shadow MMU
hooks.

### The hardware floor

`kvm-pvm` refuses to load on a host with KPTI (`X86_FEATURE_PTI`) or without
PCID and INVPCID, and on FRED hosts. That is a decision, not a gap: supporting
KPTI hosts cost a per-VM PVCS alias and KPTI-only CR3 handling in the entry
code, for CPUs that no longer ship. It was removed. LA57 is supported and
tested under TCG (`batteries/tcg.pvm`); the development machine has no LA57.

---

## Performance

### Upstream KVM against PVM

`batteries/kvm-vs-pvm.pvm`: `master` (host and guest, `kvm-intel` in L1)
against `pvm-7.3-series` (host and guest, `kvm-pvm`, direct #PF on), each a
timing build, same L1 shape, order alternating, medians of 5. Measured at
`21f9e0ced005` against `893e11787f78`, i9-13900HK, L1 with 8 vCPUs and THP:

| benchmark | KVM, 1 cpu | PVM, 1 cpu | 1 cpu | 2 | 4 | 8 |
|---|---:|---:|---:|---:|---:|---:|
| `perf/syscall` (getpid) | 58.5 ns | 211 ns | 3.60x | 3.90x | 3.71x | 3.58x |
| `perf/context-switch` (pipe) | 5147 ns | 7924 ns | 1.54x | 1.66x | 1.92x | — |
| `perf/page-fault` | 1001 ns | 3004 ns | 3.00x | 3.40x | 2.93x | 2.99x |
| `perf/parallel-fault` | 705 ns | 2737 ns | 3.88x | 3.83x | 3.73x | 4.71x |
| `perf/fork-exec` | 470 µs | 1675 µs | 3.56x | 4.82x | 4.56x | 4.58x |
| `perf/parallel-fork` | 445 µs | 1586 µs | 3.56x | 4.81x | 4.98x | 6.51x |

The two sides' ranges are disjoint everywhere except context-switch at 1 cpu.
Context-switch lost reps at 2 and 4 cpus and reports nothing at 8 — read that
row as indicative. `perf/fault-scaling` is 2.9-3.5x and the `fault-rounds`
refaults 2.7-5.2x.

**The nesting inflates PVM's numbers.** Every `vcpu_enter_guest()` reads
`MSR_IA32_DEBUGCTLMSR`, which in L1 is an exit to L0 costing ~0.8-1 µs. kvm-intel
pays it too, but PVM enters far more often (every fixed #PF and hypercall is a
full entry), so on bare metal the fault ratios would be lower. A per-CPU
DEBUGCTL shadow is the upstream-shaped fix and would help both vendors.

### What the syscall cost is made of

Decomposed at an earlier tip (PVM 240 ns), using the guest's own KPTI as a
reference for what a per-syscall CR3 switch costs on this machine:

| guest | ns/getpid | vs. KVM without guest KPTI |
|---|---|---|
| KVM, guest `pti=off` | 67.7 | 1.00x |
| KVM, guest `pti=on` | 174.0 | 2.57x |
| PVM | 240.2 | 3.55x |

So **most of PVM's syscall cost is the CR3 switch it cannot avoid.** A PVM
guest pays a KPTI-shaped syscall whether or not it enables KPTI, because the
two-root split is the only thing between guest user space and the guest kernel.
PVM's own event handling is the rest, of which roughly 13 ns is the
protection-key swap.

### What a page fault is made of

Before direct #PF, every first touch of a page cost exactly two exits: a
reflected not-present fault (the guest's own PTE is empty) and a fixed shadow
fault once the guest filled it. 22-30% of vCPU time in the fault benchmarks.

- **Direct #PF** (`PVM_FEATURE_DIRECT_PF`) removes the first one. The switcher
  delivers a user-mode not-present fault straight to the guest's event entry,
  with no exit and no shadow walk. It is spurious when the shadow entry was
  merely missing — the guest finds its PTE present and returns, and the next
  fault on that page exits normally: never twice in a row on the same page,
  never more than 4 in a row. Spurious deliveries stay under 0.25%. Gain:
  page-fault -33%, parallel-fault -36..38%, 2.0 → 1.0 exits per page.
  It is negotiated: a CPUID bit, a guest opt-in in `MSR_PVM_FEATURES_ENABLED`,
  `pvm_direct_pf=off` on the guest command line to decline. `PVCS::cr2` is
  authoritative and synced into KVM after every run.
- **The fixed fault** that remains costs 1.5-4.8 µs. Timed under
  `CONFIG_KVM_PVM_STATS`: after the shadow MMU is done, re-entry is 0.9-1.8 µs,
  dominated by the nested DEBUGCTL read above.

### What has already been won

- `perf/context-switch` **3.53x → 1.6x**: the switcher serves
  `PVM_HC_LOAD_PGTBL` without an exit, matching against up to four
  `(guest_cr3, smod_cr3, umod_cr3)` triples the hypervisor publishes into
  `tss_ex.pgtbl`. The first version halved the exits and did not get faster,
  because it killed the mode direct switch (`SWITCH_FLAGS_NO_DS_CR3`);
  publishing both roots of the pair fixed it. Any new fast path has to keep the
  others alive.
- Page faults: direct #PF, above. Before it, page-fault was 4.4x and
  parallel-fault 6-7x.
- An LA57 livelock in the NX-based SMEP emulation (kernel and user share
  `PML5[0]`), found under TCG.

---

## Ruled out, with reasons

Do not re-propose these without new evidence.

**Protection keys instead of the two-root split.** One shadow root for both
guest modes, with PKRU denying the kernel's keys in guest user mode. Dead:
`WRPKRU` is unprivileged and does not trap, so guest user code simply unlocks
the kernel's keys. This is the single biggest performance idea and it does not
work.

**PKS for the same purpose.** PKS governs supervisor-mode accesses to `U=0`
pages. M1 makes every page `U=1`. Nothing to govern.

**Sharing non-leaf shadow pages between the guest's two roots.** Fuses the
trees that `role.access & ACC_USER_MASK` separates and defeats the NX-based
SMEP emulation (M4, M3).

**Tuning `tlb_single_page_flush_ceiling` in the guest.** Swept 1-33 against
`parallel-fault`: no trend, exit counts unchanged.

**`HC_TLB_INVLPG` range batching.** 38k of the perf suite's 47k single-page
flushes happen in the first 60 ms of guest boot, and benchmark ranges average
1.1 pages. Branch `pvm-tlb-range-experiment`.

**x2APIC ICR / TSC_DEADLINE fast re-entry.** Regressed context-switch through
more HLT cycles. Branch `pvm-icr-fastpath-experiment`.

**Re-reading the guest PTE through a cached hva.** Neutral. Branch
`pvm-gpte-hva-experiment`.

**A not-present SPTE marker** to skip the reflected fault. The marker is 100%
stale when the guest fills its PTE through an unsync page, and kept only in
synced pages it is hit ≤0.02%. Only an ABI change could attack that cost —
which is what direct #PF became. Branch `pvm-np-marker-experiment`.

**Prefaulting the shadow entry at direct-#PF time, and a swapgs-only GSBASE
path.** Stopped by analysis: both need state the switcher does not have without
a walk or an exit.

**Fast re-entry for the fixed #PF.** The measured cost is in
`vcpu_enter_guest()` itself (the nested DEBUGCTL read). A PVM-private re-entry
would duplicate its request, event and FPU handling, or run MMU faults with
IRQs off. Not worth it; fix DEBUGCTL instead.

**A negotiated linear-address range** (`MSR_PVM_LINEAR_ADDRESS_RANGE`). Replaced
by "the guest owns the lower half", a sign test.

**PV MMU as an obvious win.** KVM removed its own PV MMU because it was slower
than shadow paging. Not a closed door for PVM, which has no EPT to fall back
on, but read why it failed there first.

---

## Open leads: performance

1. **`fork-exec` and `parallel-fork`, 3.6-6.5x.** The largest remaining gap
   and barely decomposed: shadow page churn on exec and exit, write-protection
   of the parent's tables on fork, `mmu_lock` contention (~400 ns/page waited at
   8 vCPUs in parallel-fault).
2. **DEBUGCTL on every entry**, in common KVM code. See above.
3. **The syscall's ~1.4x over a KPTI syscall.** The switcher does ~6 PVCS
   stores, a `swapgs` pair, `rdgsbase`/`wrgsbase`, and the guest then runs its
   own entry sequence.
4. **The protection-key swap, ~13 ns.** Unconditional for any guest with
   `CR4.PKE`; Linux tasks run on `init_pkru` = 0x55555554, so a "user PKRU
   equals supervisor" short circuit never fires. Making it free means letting
   supervisor mode run on the user's PKRU — a decision, not an optimisation.
5. **`XCR0` switching.** PVM keeps the guest's `XCR0` equal to the host's to
   avoid an `XSETBV` exit to L0 on every switch; the size of that cost has never
   been separated from run-to-run drift.

For anything else: `make exits`, then `pvmtest resolve-exits`, and look at the
top reasons with their guest RIPs. Per-case counters come from a `stats=on`
boot and `pvmtest stats <log>`.

---

## Open leads: fewer changes

`v7.3-rc2..pvm-7.3-series` at `21f9e0ced005`: 49 commits, 115 files, 11063
insertions, 295 deletions. That includes debug-only commits to drop before
upstreaming: the `CONFIG_KVM_PVM_STATS` counters, the `vcpu_enter_guest()`
stamps and the fault classification (`d7216598db68`).

| area | files | + | − | note |
|---|---:|---:|---:|---|
| `arch/x86/kvm/pvm` | 4 | 4727 | 0 | new, nothing shared |
| `Documentation` | 4 | 1630 | 0 | spec, invariants |
| `arch/x86/kernel` | 18 | 1267 | 32 | mostly PIE + the guest side |
| `arch/x86/entry` | 9 | 1135 | 21 | switcher (new) + hooks |
| `arch/x86/include` | 26 | 1038 | 69 | |
| `arch/x86/kvm/mmu` | 5 | 312 | 30 | the shared shadow MMU |
| `arch/x86/kvm` (other) | 10 | 230 | 29 | mostly the debug stamps |
| `tools` | 9 | 226 | 13 | objtool, perf, one selftest |
| `arch/x86/mm` | 9 | 94 | 30 | |
| the rest | 25 | ~400 | ~70 | PIE: relocs, Kconfig, xen, pvh, bpf, power |

Targets, in the order a reviewer would care:

1. **The PIE kernel is the biggest shared chunk.** It exists because a PVM guest
   lives in the lower half and a `-mcmodel=kernel` image cannot. Ask whether all
   of `CONFIG_X86_PIE` is needed, or whether a smaller "relocatable to the lower
   half" change would do. It is independent of PVM and arguably wants its own
   upstream life. The `xen/`, `power/`, `platform/pvh/` and `bpf` changes are all
   PIE consequences, not PVM ones.
2. **The shared shadow MMU**, the part upstream will scrutinise hardest. Three
   globals (`shadow_force_user_mask`, `shadow_emulate_smep_with_nx`,
   `shadow_pkey_mask`) that stay zero for every other vendor, the protection key
   threaded to the SPTE, and `mmu_adjust_kernel_only_access()`. Ask whether the
   three globals want to be one.
3. **The entry hooks**: tests of `TSS_extra(host_rsp)` in `entry_64.S`, the CR3
   macros in `calling.h` and the direct #PF jump in `asm_exc_page_fault`, all
   under `CONFIG_X86_PVM_SWITCHER`. The KPTI variants are gone.

The invariants document also records what earlier simplification removed, which
is the shape of a successful reduction: moving the guest kernel into the lower
half retired invariant M7, the range MSR, four per-vCPU bounds, the
`get_vm_area_align()` reservation, the PML bookkeeping and the LA57 top-p4d
merge in one move.

---

## What is missing or open functionally

- **PKS, and therefore SMAP for the guest.** Supervisor mode runs on `PKRU` 0
  rather than on the guest's `MSR_IA32_PKRS`. See "Protection Keys" in
  `pvm-spec.rst`.
- **FRED hosts** are refused: the switcher owns the IDT entry paths.
- **`set_memory_region_test` fails under PVM** and passes under kvm-intel.
  Unexplained; recorded in `configs/kvm-selftests-expect.txt` with the other
  per-vendor outcomes (`kvm_pv_test`, `cr4_cpuid_sync_test`).
- **`security/pkey-allowed` fails intermittently under TCG** (4-level and LA57,
  direct #PF on or off) and never under KVM (0 of 200). Believed to be TCG;
  `batteries/pkey-tcg.pvm` repeats it with PKRU printed on each failure.

---

## How to measure without fooling yourself

Every one of these cost real time at least once.

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
- **Metric lines must end with `#END`.** Interleaved `printk` once corrupted a
  metric line into a plausible wrong ratio; the parser ignores lines without it.
- **Single runs drift by tens of percent.** Medians of 5, and look at whether
  the ranges overlap.
- **Whole-suite exit histograms include guest boot.** Use per-case marks.
- **L1 has 8 vCPUs**: 16 guest vCPUs are overcommitted, not a scaling point.
  L1 has THP; without it kvm-intel's page-fault was 9 µs and PVM looked faster.
- **`pr_info` from the module does not reach L1's live console during a guest
  run; `pr_emerg` does.**
- **`L1_QMP=<path>`** opens a QMP socket on L1's QEMU. `info registers -a` on a
  wedged L1 is how a livelock was found after hours of guessing.
- **`SELFTESTS=<glob>`** stages only the KVM selftests you are asking about.
- **`pvmtest.guest_timeout=<s>`** shortens the wait for a hanging guest.
- **Under TCG**, INVD/WBINVD do not fault at CPL 3 and `time/monotonic` times
  out. Emulator artefacts; nothing measured under TCG means anything.

## The ABI affordance

`PVM_CPUID_SIGNATURE` / `PVM_CPUID_FEATURES` (0x40000200) carry an ABI version
and a feature bitmap, and the guest refuses a version it was not built for.
Optional behaviour takes a `PVM_FEATURE_` bit, which the guest enables in
`MSR_PVM_FEATURES_ENABLED` (per vCPU, 0 at reset, #GP on unknown bits), and
needs no version bump. `PVM_FEATURE_DIRECT_PF` (bit 0) is the first.
