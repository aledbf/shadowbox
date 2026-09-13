# PVM next performance pass — measurement log

Follows `docs/pvm-tuning-v2.md`.  The previous pass is `docs/TUNING-LOG.md`.

## Baseline

| | |
|---|---|
| host CPU | i9-13900HK, SMT on, governor powersave, 20 threads, no LA57 |
| L0 kernel | 7.0.0-31-generic |
| L1 (PVM host) | 8 vCPUs, 8G, KPTI off ("Not affected"), 4-level paging |
| PVM commit | `86c154bbb372`, host and guest |
| guest | 1G, perf suite, 1/2/4/8/16 vCPUs |
| repetitions | 5, medians, vendors interleaved |
| file | `baselines/v2-86c154bbb372.tsv` (also the env baseline) |

PVM / kvm-intel:

| benchmark | 1 | 2 | 4 | 8 | 16 |
|---|---:|---:|---:|---:|---:|
| syscall | 3.54x | 3.73x | 3.61x | 3.61x | 3.43x |
| context-switch | 1.53x | 1.91x | 2.34x (1 run) | — | — |
| page-fault | 0.81x | 0.70x | 0.77x | 0.75x | 0.76x |
| fork-exec | 3.73x | 3.86x | 4.96x | 3.50x | 3.05x |
| parallel-fork | 3.94x | 3.82x | 4.87x | 6.75x | 5.14x |
| parallel-fault | 6.43x | 4.14x | 10.36x | 7.32x | 4.73x |

parallel-fault ns/page, PVM: 4517, 2517, 3324, 1614, 1631; kvm-intel: 702.5,
607.5, 320.7, 220.4, 344.6.  Spreads on PVM up to 93%.

L1 has 8 vCPUs, so 16 guest vCPUs are overcommitted on L1 for both vendors:
the 16 column is not a scaling point.

## Phase B — HC_WRMSR accounting

Kernel `722ec0cc66e2` (counters) on `a15d1cf2eb40` (per-case snapshots:
the guest writes `MSR_PVM_STATS_MARK` around each case, `case-stats.sh` diffs
the host's tagged prints).

Per case, 2 vCPUs, counting build (the ICR fast path was also in this build;
`hc_wrmsr` counts the slow path only, so ICR shows as `wrmsr_fast_icr_success`):

| case | HC_WRMSR (slow + fast) | ICR | TSC deadline | EOI | other |
|---|---:|---:|---:|---:|---:|
| syscall | 686 | 64 | 621 | 0 | 1 |
| context-switch | 90,470 | 74,433 | 16,034 | 0 | 3 |
| page-fault | 1,007 | 91 | 915 | 0 | 1 |
| fork-exec | 6,991 | 2,302 | 4,687 | 0 | 2 |
| parallel-fork | 1,563 | 73 | 1,489 | 0 | 1 |
| parallel-fault | 498 | 55 | 442 | 0 | 1 |

Reconciles: total = ICR + TSC deadline + EOI + other, from one run.  ICR is a
context-switch phenomenon; nothing else makes enough of either MSR to matter.
EOI never goes through the hypercall at all.

## Phase C — x2APIC ICR fast re-entry: rejected

Kept on kernel branch `pvm-icr-fastpath-experiment`: `KVM: x86: split the
WRMSR fastpath's write from its instruction skip` (generic, exports
`kvm_fastpath_wrmsr_write()`) and the PVM re-entry.

Hypothesis: an ICR hypercall spends avoidable time in handle_exit and the
outer vcpu_enter_guest() loop; serving it at the end of `pvm_vcpu_run()` with
KVM's own fastpath write and returning `EXIT_FASTPATH_REENTER_GUEST` makes each
one cheaper.

Normal path: `handle_exit_syscall()` -> `handle_hc_wrmsr()` ->
`kvm_emulate_msr_write()`.  Fast path: `kvm_fastpath_wrmsr_write()` (the
code VMX uses), RAX = 0, re-enter.

Semantics preserved:
- CPUID: x2APIC mode checked by the helper;
- MSR filter: x2APIC MSRs cannot be filtered;
- tracing: `kvm_msr` write traced by the helper, `kvm_exit` still emitted;
- APIC state: same `kvm_x2apic_icr_write_fast()` as VMX;
- userspace exits: none possible for ICR; pending requests end the loop.

Note why the exported VMX helper could not be called directly:
`handle_fastpath_wrmsr_imm()` ends in `kvm_skip_emulated_instruction()`, and
PVM's skip decodes and skips the instruction *after* the hypercall's
SYSCALL, whose exit has already moved RIP.

Fastpath hit rate: 100% (74,433 of 74,433 in context-switch at 2 vCPUs,
101,102 of 101,102 at 8).

A/B/A, non-counting builds, 5 reps, pvm only:

| case | vCPUs | A1 (no fastpath) | B (fastpath) | A2 (no fastpath) |
|---|---:|---:|---:|---:|
| context-switch ns | 2 | 7,770 (7,495-8,138, n=3) | **11,120** (9,614-11,290) | 7,416 (6,672-9,846) |
| fork-exec us | 2 | 2,168 | 2,182 | 2,285 |
| parallel-fork us | 8 | 642.8 | 648.2 | 636.2 |
| parallel-fault ns | 8 | 1,757 | 1,444 | 1,470 |
| syscall ns | 2 | 212.6 | 207.5 | 208.2 |

The one workload with many ICR writes regressed; nothing else moved outside
its spread.  Counters say why (context-switch, 2 vCPUs, same 50,000 round
trips):

| | no fastpath (2 runs) | fastpath |
|---|---:|---:|
| ICR writes | 54,367 / 45,773 | 74,433 |
| HLT exits | 54,395 / 45,176 | 72,742 |
| exits | 134,787 / 122,623 | 190,428 |

Returning to the guest sooner changed how the IPI's sender and its halted
receiver interleave: more IPIs, more HLT exits (the expensive kind, tens of
microseconds each), more total work.  The per-exit saving is real and is
swamped.  Decision: revert (plan C6).  Phase D (TSC_DEADLINE) was gated on C
proving the approach and is not started.

What would be worth knowing before trying again: why a faster IPI send leads
to more halt cycles -- halt polling (`halt_poll_ns`) behaviour for PVM, and
which vCPU halts.

## Phase A — the PVCS alias, hardened

Testbed host tests (`hosttests/pvm_switcher_test.c`), each run on a `pti=on`
host (alias in use) and without KPTI (direct map):

- `pvm/pvcs-alias-rebind`: the PVCS is moved to another page three times
  between exits, the old page poisoned each time; 200 rounds after each move
  must stay clean direct switches.  A stale translation would send user mode
  to the poison.  Pass, both.
- `pvm/pvcs-alias-migrate`: 20,000 rounds while another thread moves the vCPU
  thread to a different CPU every millisecond (104 moves); 20,035 exits.  Pass,
  both.
- `pvm/pvcs-alias-last-vcpu`: 1024 vCPUs (KVM_CAP_MAX_VCPUS), each with its own
  patterned PVCS page; only vCPU 1023 runs, 200 direct-switch rounds in 200
  exits; every other PVCS page untouched afterwards; vCPU 1025 refused.  Pass,
  both.  Bounds: the alias region is one PGD slot, and
  `static_assert(KVM_MAX_VCPUS <= PTRS_PER_PMD * PTRS_PER_PTE)` in host_mmu.c
  keeps every index inside the one PMD page the branch has.

LA57 (A1): not done yet -- this CPU has no LA57; `run-l1.sh` can now boot L1
under TCG with `L1_ACCEL=tcg L1_CPU=max,la57=on` for it.

## Phase F — attributing the parallel-fault scaling loss

### First-touch faults alone: `perf/fault-scaling`

New case: regions mapped outside the clock, workers released together, only
the loop writing one byte per fresh page timed.  Counting build (absolute
numbers inflated), ns per page times vCPUs -- flat would be perfect scaling:

| vCPUs | PVM (2 runs) | kvm-intel ept=0 | kvm-intel ept=1 |
|---:|---:|---:|---:|
| 1 | 5,713 / 6,277 | 18,293 | 3,344 |
| 2 | 7,249 / 6,297 | 21,855 | 5,253 |
| 4 | 8,115 / 21,388 | 31,800 | 4,658 |
| 8 | 10,406 / 8,885 | 29,086 | 7,230 |

Mechanism counters, PVM, per case (case-stats deltas, run 1):

| metric | 1 | 2 | 4 | 8 |
|---|---:|---:|---:|---:|
| pages | 32,768 | 65,536 | 131,072 | 262,144 |
| PF exits / page | 2.23 | 2.32 | 2.31 | 2.31 |
| PF fixed / page | 1.23 | 1.31 | 1.31 | 1.31 |
| PF guest-injected / page | 1.00 | 1.00 | 1.00 | 1.00 |
| PF retry / page | 0 | 0 | 0 | 0 |
| mmu_lock acquisitions contended | 0% | 7.9% | 21.5% | 46.7% |
| mmu_lock wait, ns / acquisition | 0 | 19 | 76 | 300 |
| mmu_lock hold, ns / acquisition | 210 | 310 | 346 | 390 |
| mmu_lock wait, ns / page | 0 | 25 | 100 | 392 |
| remote TLB flush / page | 0 | 0.0011 | 0.0017 | 0.0021 |
| shadow cache misses / page | 0.0027 | 0.0027 | 0.0023 | 0.0026 |

`perf lock` (tracepoints, heavier) agrees on which lock -- mmu_lock taken for
write from the shadow page fault, 300k contended acquisitions at 8 vCPUs --
and reads a larger total wait (1.1 s) that its own recording inflates.

Reading: no retry storm, no allocation churn, no remote flush growth (cases
2-4 of the plan's tree are out).  mmu_lock contention is real and grows with
concurrency, but at 8 vCPUs it is ~400 ns of the ~3,600 ns per page that
scaling loses.  kvm-intel with ept=0 -- the same shadow MMU -- degrades by the
same factor, and with EPT by a larger one: the environment (8 L1 vCPUs on a
6-core, 12-thread set of L0 P-cores, SMT siblings sharing cores) accounts for
most of the loss, for every vendor.  PVM's first-touch fault is 1.2-1.9x a
TDP guest's here and ~3x faster than a shadow-paging VMX guest's.

So the 4-10x of parallel-fault is not first-touch faulting.  What that case
adds is concurrent munmap from one process -- guest TLB shootdowns and the
host zapping shadow pages -- and a read-back pass.

### Concurrent munmap alone: `perf/unmap-scaling`

Regions mapped and touched outside the clock, all workers munmap together,
only that timed.  ns per page (ns per page times vCPUs):

| vCPUs | PVM | kvm-intel ept=1 | kvm-intel ept=0 |
|---:|---:|---:|---:|
| 1 | 72 (72) | 96 (96) | 96 (96) |
| 2 | 95 (189) | 92 (183) | 131 (262) |
| 4 | 91 (364) | 79 (314) | 141 (564) |
| 8 | 135 (1,082) | 99 (789) | 148 (1,180) |

Unmapping is not where PVM loses either: tens of nanoseconds a page, and
within 1.4x of a TDP guest.

### What parallel-fault's ratio actually is

A TDP guest faults on its host side only the first time a guest physical page
is used; parallel-fault's six rounds reuse the same guest memory, so from the
second round on KVM with EPT takes no exits at all for them.  A shadow-paging
guest's SPTEs follow the guest's page tables, so every fresh mapping costs the
same ~2.3 exits per page again.  The 4-10x is mostly that, not a scaling
collapse: fault-scaling, whose clock also holds the TDP guest's first-use EPT
faults, reads 1.2-1.4x at 8 vCPUs, and unmap-scaling 1.4x.  (That comparison
favours PVM somewhat -- how much of fault-scaling's kvm-intel time is EPT
population is not separated here.)

### The extra 0.3 fault per page: the guest's direct map, and THP

Counters split by guest mode (`e5357bf1c0e9`), fault-scaling at 8 vCPUs:
80,326 #PF exits in supervisor mode against 524,494 in user mode.  The guest
kernel zeroes each new page through its direct map, and the first touch of a
page there is a shadow fault of its own.  L1 has no `CONFIG_TRANSPARENT_HUGEPAGE`,
so every SPTE the shadow MMU builds is 4K even where the guest maps 2M or 1G.

Same guest, counting builds, L1 with and without THP:

| | no THP, 2 | THP, 2 | no THP, 8 | THP, 8 |
|---|---:|---:|---:|---:|
| fault-scaling supervisor #PF exits | 19,580 | 373 | 80,326 | 2,197 |
| PF fixed / page | 1.30 | 1.004 | 1.30 | 1.007 |
| exits | 153,330 | 133,845 | 614,866 | 535,948 |
| mmu_lock wait, ms | 1.09 | 0.56 | 87.3 | 49.2 |
| fault-scaling ns/page x vCPUs | 6,008 | 5,285 | 9,345 | 8,608 |
| page-fault ns/fault | 7,326 | 5,858 | 4,538 | 4,489 |
| KVM pages_2m at mark | 0 | 46 | 0 | 144 |

An environment finding rather than a PVM change: a host with THP (every
production host) does not pay it.  The testbed's L1 does, and every number in
both passes includes it.

### Contention in parallel-fault

`perf lock`, parallel-fault, 8 vCPUs (non-OOM run): 219,507 contended
acquisitions of mmu_lock for write, 980 ms total wait, all from the shadow page
fault; sync paths barely appear (kvm_mmu_load 978, kvm_vcpu_flush_tlb_guest 5).
The counting build's own timer: 331 ms, ~1,680 ns per page -- four times what
fault-scaling waits, because the zaps and the faults of the other workers'
rounds overlap.  Fault against fault, not flush against fault.

Self-time profile of the same case: lock slow paths 10% (queued_spin 6.5,
queued_write 3.5); guest page table reads 7% (`__kvm_read_guest_page` 3.4,
`kvm_vcpu_read_guest_atomic` 1.3, `kvm_vcpu_gfn_to_memslot` 1.2,
`__get_user_8` 0.65, walk 0.7).  The 34% on `asm_fred_entry_from_kvm` is guest
time: PMIs taken in the guest are replayed through `x86_entry_from_kvm()`
after the exit, with host registers.

Earlier profiles of fault-scaling with `pvmtest.scale=4` and `=16` ran the
guest out of memory (1-4 GB asked of a 1 GB guest) and are discarded.

## §19 — stack_trace_save() in a PVM guest: not a PVM problem

The empty traces from the TLB experiment were taken in the first 60ms of guest
boot, before `unwind_init()` has run.  After boot, ORC unwinds normally in a
PVM guest: an OOM report in a later log carries a complete call trace
(`dump_stack_lvl` / `out_of_memory` / `__handle_mm_fault` /
`do_user_addr_fault` ...).  Closed.

## §20 — userspace_msr_exit_test

Kernel `f99cff29132f`: `msr_filter_allow` skips itself when `kvm_pvm` is
loaded, saying why (a selftest guest registers no PVM event entry, so the #GP
it asks KVM to inject can only be a triple fault).  VMX/SVM untouched; the
other three cases still run.
