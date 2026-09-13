# PVM optimisation pass — measurement log

Follows `Documentation/virt/kvm/x86/pvm-tuning.md` in the kernel tree.  One
section per phase, each with the note that document asks for.  Numbers are
from this machine only; see "Baseline" for what it is.

## Baseline (Phase 0)

| | |
|---|---|
| host | i9-13900HK, SMT on, governor powersave, L0 kernel 7.0.0-31-generic |
| host KPTI (L1) | off ("Not affected") |
| kernel under test | `pvm-7.3-series` at `c39e89576664`, host and guest |
| guest | same tree, perf suite, 1G |
| repetitions | 5 per point, medians, vendors interleaved |
| file | `baselines/7.0.0-31-generic__13th-Gen-Intel-R-Core-TM-i9-13900HK.tsv` |

PVM / kvm-intel (both inside L1):

| benchmark | 1 cpu | 2 | 8 | 16 |
|---|---:|---:|---:|---:|
| syscall ns/getpid (PVM) | 228.0 | 216.1 | 234.9 | 212.5 |
| syscall ratio | 3.79x | 3.75x | 3.69x | 3.54x |
| context-switch | 1.53x | 1.96x | — | — |
| page-fault | 0.90x | 0.76x | 0.78x | 0.74x |
| fork-exec | 3.69x | 4.31x | 3.64x | 2.90x |
| parallel-fork | 4.00x | 4.03x | 7.15x | 4.94x |
| parallel-fault | 5.97x | 3.96x | 5.82x | 4.49x |

Spreads (max-min over median) reach 46% on parallel-fault and 35% on
page-fault; do not read a single row of those alone.  8 of the 40 runs had
their result line cut by L1's console and were dropped by the old
perf-matrix; their metrics were complete and are included (b7082a0).

### Whole perf suite, 2 vCPUs, one run (counting build, `l1-perf-pvm-stats.log`)

`CONFIG_KVM_PVM_STATS` agrees with `perf kvm stat` to the exit: 46883
`HC_TLB_INVLPG`, 2289 `HC_LOAD_PGTBL`, 1816 `ERETU`, 783766 entries against
783764 samples.

| | count | share of exits |
|---|---:|---:|
| total exits | 783,764 | |
| PF excp | 506,853 | 64.7% |
| HC_WRMSR | 91,495 | 11.7% |
| HC_TLB_INVLPG | 46,883 | 6.0% |
| HC_LOAD_PGTBL | 2,289 | 0.3% |
| ERETU (direct-switch fallback) | 1,816 | 0.2% |
| syscall from user via hypervisor | 0 | |

Mechanism counters over the same run:

- direct switches: 2,617,875 to supervisor, 2,800,311 to user;
- LOAD_PGTBL served by the switcher: 103,590 paired, 499 unpaired; 2,289 not,
  1,550 of those because of the flags word (TLB flush requested);
- ERETU fallbacks: 1,703 NO_DS_CR3, 113 another inhibitor, 0 selectors;
- PGTBL publication: on every one of the 783,766 entries, 3,379,416 roots
  inspected (4.3 per entry), 1,187,253 table entries filled;
- `HC_TLB_INVLPG`: 35,147 of 46,883 (75%) are the page after the previous one
  with no exit in between -- decomposed ranges;
- PKRU: every RDPKRU read a non-zero user PKRU (2,617,875 of 2,617,875).  A
  Linux task runs on `init_pkru` = 0x55555554, so the "user PKRU is 0 / equals
  smod_pkru" short circuit of Phase 5 would never fire for a Linux guest.
- `HC_WRMSR`, by MSR: 67,280 x2APIC ICR (0x830), 23,765 TSC_DEADLINE (0x6e0),
  3,001 EOI (0x80b).  IPIs and the timer, not anything the switcher could own;
  KVM serves both in its exit fastpath for VMX, and PVM always returns
  `EXIT_FASTPATH_NONE`.  Not in the plan; the largest exit reason after page
  faults.

Per case (`make exits`-style deltas, 2 vCPUs, case minus a no-case boot):

| | parallel-fault | context-switch | syscall | fork-exec | page-fault | parallel-fork |
|---|---:|---:|---:|---:|---:|---:|
| exits | 102,159 | 130,688 | 3,470 | 164,828 | 188,789 | 61,438 |
| HC_TLB_INVLPG | ~0 | 2,333 | 868 | 4,831 | noise | 2,800 |
| LOAD_PGTBL served / not | 0 / 0 | 103,032 / 60 | 0 / 0 | 629 / 1,793 | 0 / 8 | 337 / 353 |
| ERETU fallback | 21 | 76 | 0 | 1,577 | 4 | 277 |

(The per-case counter columns were halved from a run that counted each
PVMSTATS line twice; ffb4af9 fixed the script.)  The plan's "parallel-fault:
~40k HC_TLB_INVLPG" does not hold at the series tip: that case makes almost
none.  Its exits are page faults.

## Phase 1 — stale NO_DS_CR3

Kernel `5b2a8abd32bd`, testbed `46005d6`.

Hypothesis: after the switcher serves LOAD_PGTBL with a paired root,
`NO_DS_CR3` left over from the previous address space keeps the mode direct
switch off until the next hypervisor entry.

Mechanism changed: `PGTBL_TRY` clears the bit when the entry has a user-side
root.

Expected counter movement: `eretu_exit_no_ds_cr3` down, `ds_to_umod` up, in
exactly the sequence "unpaired address space, then paired one via the
switcher".

Evidence, `pvm/pgtbl-fastpath-clears-no-ds-cr3` (200 rounds):

| kernel | exits, detour | exits, control | eretu_exit_no_ds_cr3 | ds_to_umod |
|---|---:|---:|---:|---:|
| fix reverted | 600 | 400 | 205 | 203 |
| fix | 400 | 400 | 1 | 407 |

The first version of the case passed on the broken kernel: its guest had no
PCIDE, so LOAD_PGTBL with the TLB bit clear became a CR3 with a reserved bit,
`kvm_set_cr3()` refused it, and `handle_hc_load_pagetables()` ignored the
refusal.  The counters showed it (`pgtbl_published_paired=0` while every hit
was table entry 0).

Side finding: `handle_hc_load_pagetables()` dropped the return value of
`kvm_set_cr3()`, so an invalid CR3 load was silently a no-op for the guest.
Fixed in Phase 11.

Benchmark before/after: not yet run on a non-counting build (the real-guest
sequence is rare: 1,703 NO_DS_CR3 ERETU fallbacks in a whole suite, and not all
of them are this sequence).  Expected neutral on the matrix.

## Phase 2 — observability

Kernel `bab16a13fba6` (`CONFIG_KVM_PVM_STATS`, default n).  Build a counting
host with `HOST_CONFIG_EXTRA=CONFIG_KVM_PVM_STATS=y`; perf-matrix refuses one.
Every number in the Baseline counter tables above comes from it.

## Phase 3 — range TLB invalidation: rejected

Kept on kernel branch `pvm-tlb-range-experiment` (three commits on top of
the counters): `PVM_HC_TLB_INVLPG_RANGE(start, nr, shift)` behind
`PVM_FEATURE_TLB_RANGE`, a `pv_ops.mmu.flush_tlb_user_range` hook used by
`flush_tlb_func()` and `do_kernel_range_flush()`, and the guest using it with
`pvm_tlb_range_chunk=` for the chunk-size experiment.  Functionally clean
(`stage2` passed on it).

Hypothesis: the ~47k `HC_TLB_INVLPG`, 75% of them sequential, are ranges the
guest decomposed, and one exit per range instead of per page removes most of
them.

What the counters said (whole perf suite, 2 vCPUs, counting build):

| guest | HC_TLB_INVLPG | sequential | range HCs | pages in range HCs |
|---|---:|---:|---:|---:|
| tip (no range) | 46,883 | 35,147 | — | — |
| user ranges, chunk 512 | 37,885 | 34,786 | 8,510 | 9,623 |
| user ranges, chunk 1 | 37,881 | 34,779 | 9,955 | 9,955 |
| + kernel ranges, chunk 512 | 37,877 | 34,789 | 8,518 | 9,779 |

Ranges from `flush_tlb_func()` average 1.13 pages; batching them saves ~1.4k
exits per suite.  The sequential stream did not move at all.  A guest printing
every 2048th single-page flush showed why: all of them fall in the first 60ms
of guest boot, at 0x7fffff200000-0x7fffff20e000 -- early fixmap/early_ioremap
slots, reused -- before any benchmark runs.  The suite histogram counts boot.

Stop condition met ("correctness requires ... before we have proved exit
amortization helps" / "exits fall but the benchmark does not improve"): the
exits it targets are not in the workloads.  Not worth an ABI addition and a
generic x86 hook.  If boot time ever matters, the thing to look at is what
early boot remaps one page at a time, not the flush.

Two things noticed while doing it:
- `stack_trace_save()` returns no frames in a PVM guest (ORC, PIE, lower
  half).  Unexplained.
- The plan's "parallel-fault: 40k HC_TLB_INVLPG" was a suite-level count
  including boot, or predates the tip.

## Phase 4 — PGTBL publication

Kernel `084e0eab7b17`: one pass sorts prev_roots[] into supervisor and user
roots, pairing is by pgd.  At most 3 + 6 `is_root_usable()` calls per entry
instead of 3 + 15.  `stage2`, hosttests clean; the switcher's work unchanged
within run-to-run variation (paired loads served 105,141 vs 103,590, ERETU
fallbacks 1,665 vs 1,816, LOAD_PGTBL exits 2,168 vs 2,289).

Not kept separately in the matrix: the loop is nanoseconds of an exit that
costs ~2us, and a host cycles profile (`run-l1.sh profile`, non-counting
build) does not show `pvm_publish_pgtbl_cache` among the top symbols -- the
profile is dominated by boot-time instruction emulation, which it cannot
separate from the case.  Rebuilding only on root-state change was therefore
not attempted: no measured cost to recover, and the always-rebuild table is
what makes stale roots impossible.

## Phase 5 — PKRU: the simple fast path does not exist for Linux

Counted: 2,617,875 RDPKRUs on the way into supervisor mode over the suite,
every one of them non-zero.  Linux gives every task `init_pkru` =
0x55555554 (all keys but 0 access-disabled), and `smod_pkru` is 0, so
"user PKRU == smod_pkru" and "user PKRU == 0" never hold and a skip based on
either would never fire.  Implementing it would add a branch and save nothing.

What would save the WRPKRUs is the plan's second-stage idea in its simplest
form: let supervisor mode run on the user's PKRU whenever that permits every
key supervisor pages use -- key 0 for a Linux guest, checkable as
`(pkru & 3) == 0` plus a sticky "a supervisor leaf SPTE ever carried a non-zero
key" bit from the shadow MMU.  It is not a pure optimisation: the guest
kernel's accesses to *user* pages would then be subject to the user's PKRU
(which is what real hardware does, and what PVM does not do today with
`smod_pkru` = 0), and to the previous task's PKRU between the guest's
context switch and its return to user mode.  That is a change to the
emulated architecture, with the pkey security cases to re-argue, for at most
the ~13 ns measured.  Left for a decision rather than done.

## Phase 6 — the shared shadow MMU

Kernel `fb1ce3999e0a`: the guest's protection key is applied after
`make_spte()` in the two shadow paths that have a guest PTE (`mmu_set_spte()`,
`FNAME(sync_spte)()`), so `make_spte()` has its upstream signature and
`tdp_mmu.c` is untouched by the series; `sync_spte()` no longer extracts the
key when `shadow_pkey_mask` is zero.  stage2, security (pkey cases included),
host tests and the KVM selftest subset (x86/state_test included) as recorded.

`disallowed_va` stays.  It is `KVM_X86_OP_OPTIONAL_RET0` through
`static_call`, which for a vendor that does not implement it is patched to an
inline `xor %eax,%eax` at the call site: no indirect call for kvm-intel or
kvm-amd.  Moving the check to PVM's #PF exit would cover the path that
installs SPTEs but not the emulator's `gva_to_gpa` walks, which today refuse
an upper-half address too.  Keeping M7's coverage beats removing a
five-byte instruction.

## Checkpoint matrix: phases 1 + 4 + 6

Full matrix at `fb1ce3999e0a` against the Phase 0 baseline: everything within
spread except two points flagged by the 15% rule, parallel-fault at 8 vCPUs
(+32%, spread 88%, min 1093 below the baseline's min) and at 16 (+21%, 4 of 5
runs above the baseline's max).  Phase 6 is on the fault path, so an A/B/A at
16 vCPUs, 6 reps each, same guest, host with and without Phase 6:

| host | parallel-fault median | min | max |
|---|---:|---:|---:|
| A1: with Phase 6 | 1706 | 1473 | 1943 |
| B: Phase 6 reverted | 1657 | 1501 | 2217 |
| A2: with Phase 6 | 1786 | 1361 | 1855 |

Overlapping; not Phase 6.  The whole machine read slower than at the baseline
(kvm-intel at 16 vCPUs 352.9 -> 374.9 ns too).  Neutral, as expected for
these three.

## Phase 7 — direct switching under host KPTI

Kernel `8eb2f4c62dcf` (LDT remap slot kept out of the root template) and
`5316c2d7ff7c` (the alias).

Not the plan's "fixed per-CPU address in the CPU entry area".  The CPU entry
area is mapped in every host user page table and every other VM's roots, and
host KPTI is on by default exactly where Meltdown reads supervisor mappings
from CPL 3: a PVCS there would leak guest register state to every host
process and every other guest.  Instead each VM gets its own copy of the root
template with a private branch in the LDT remap PGD slot mapping each vCPU's
PVCS at `LDT_BASE_ADDR + vcpu_idx` pages.  Only that VM's roots map it; no
per-CPU remapping on vCPU migration; a changed translation retires the vCPU's
ASID.  Details in the commit and in S9.

Host booted `pti=on`, 2 vCPUs, 3 reps, medians (non-counting builds):

| benchmark | before | after | after, no KPTI |
|---|---:|---:|---:|
| syscall ns/getpid | 2,792 | 212.8 | 207.1 |
| context-switch ns | 51,180 | 7,940 | 8,634 |
| fork-exec us | 3,271 | 2,250 | 2,319 |
| parallel-fork us | 1,533 | 947 | 942.8 |
| parallel-fault ns | 3,098 | 2,389 | 2,324 |

Counting builds, whole suite under `pti=on`:

| | before | after |
|---|---:|---:|
| exits | 6,139,099 | 728,500 |
| ERETU exits | 2,780,825 | 1,571 |
| SYSCALL exits (user -> supervisor) | 2,593,156 | 0 |
| direct switches to supervisor / to user | 0 / 0 | 2,581,320 / 2,762,282 |

Tests on `pti=on`: default, security, profile (NMIs into the switcher's CPL0
window), host tests (S1, S2, S4 and the pgtbl case now on the direct switch).
Without KPTI: default and host tests.  Sanitizer clean on all.  Not tested:
5-level paging.

## Phases 8-10 — not undertaken

The plan gates them on a profile showing their cost; none did.  Per-entry
`tss_ex` writes are seven stores in an exit of ~2 us; the unconditional PVCS
dirty mark is a memslot lookup when dirty logging is off; the host entry
path checks were not visible.  The host cycles profile available here
(`run-l1.sh profile`) cannot separate a case from the guest's emulated boot,
so it could not have justified them either.

## Phase 11 — warnings and selftests

- `238692dc3440`: kvm-pvm left 3 mandatory x86 ops, 5 nested ops and 2 PMU
  ops NULL -- ten WARNs at insmod, of which the testbed only ever showed
  seven (dmesg tail).  Two of the three x86 ones were live bugs:
  `get_cpl_no_cache()` on every preempted vcpu_put() returned whatever was in
  the register, and `recalc_intercepts()` was a NOP by accident rather than
  by design.  insmod is silent now.
- `86c154bbb372`: `PVM_HC_LOAD_PGTBL` with a CR3 KVM refuses raises #GP
  instead of silently not switching.
- `x86/userspace_msr_exit_test`: explained, not fixable from PVM (the
  selftest guest has no PVM event entry to receive the #GP KVM injects).

## Open items found on the way

- `HC_WRMSR` (ICR, TSC_DEADLINE): exit fastpath for PVM.
- ~~`handle_hc_load_pagetables()` ignores `kvm_set_cr3()` failure.~~ Fixed
  in Phase 11.
- A guest without PCIDE sends LOAD_PGTBL with the TLB bit set on every
  context switch (`~val >> 63`), which the switcher never serves.  Linux on
  x86-64 with PCID hardware has PCIDE; worth a check on hosts without PCID.

# Final report

## Commits (linux-aledbf, pvm-7.3-series, on c39e89576664)

- `5b2a8abd32bd` x86/pvm: clear a stale NO_DS_CR3 when the switcher loads a paired root
- `bab16a13fba6` KVM: x86/pvm: count the fast paths and the exits that fall back from them
- `084e0eab7b17` KVM: x86/pvm: pair published roots without a second scan of prev_roots[]
- `fb1ce3999e0a` KVM: x86/mmu: keep the guest's protection key out of make_spte()
- `8eb2f4c62dcf` KVM: x86/pvm: keep a host task's LDT remap out of the root template
- `5316c2d7ff7c` KVM: x86/pvm: direct switch on KPTI hosts through a per-VM PVCS alias
- `238692dc3440` KVM: x86/pvm: supply the vendor operations KVM requires
- `86c154bbb372` KVM: x86/pvm: raise #GP when PVM_HC_LOAD_PGTBL names a CR3 KVM refuses

Each builds on its own (host config; the tip also with CONFIG_KVM_PVM_STATS),
no new compiler or objtool warnings against c39e89576664.  Not pushed.

## Correctness

- PVM focused tests: regress, stage2 (37/37), host tests (17 + 4 incl. the new
  pgtbl case), security on KVM and PVM, failclosed, and default/security/
  profile/host tests again on a `pti=on` host: all pass.
- KVM selftests: the 15-test subset as recorded, both vendors.
- Warnings: insmod silent (was 10 WARNs).  Sanitizer clean on every log of
  this pass; two `*-probe*` logs from before it carry Oopses and fail
  `make sanitize` on their own.
- Known failures: `x86/userspace_msr_exit_test` under PVM, explained (the
  selftest guest has no PVM event entry for the #GP KVM injects).

## Baseline and final matrix (host KPTI off)

baseline `c39e89576664`, final `86c154bbb372`, 5 reps, medians, PVM / KVM:

| benchmark | cpus | PVM before | PVM after | before ratio | after ratio |
|---|---:|---:|---:|---:|---:|
| syscall ns | 1 | 228.0 | 211.4 | 3.79x | 3.67x |
| syscall ns | 2 | 216.1 | 218.8 | 3.75x | 3.77x |
| syscall ns | 8 | 234.9 | 214.3 | 3.69x | 3.40x |
| syscall ns | 16 | 212.5 | 232.8 | 3.54x | 3.83x |
| context-switch ns | 1 | 8739 | 8497 | 1.53x | 1.53x |
| context-switch ns | 2 | 8480 | 8752 | 1.96x | 2.05x |
| page-fault ns | 2 | 6645 | 6651 | 0.76x | 0.74x |
| fork-exec us | 2 | 2306 | 2324 | 4.31x | 3.96x |
| fork-exec us | 16 | 3925 | 3921 | 2.90x | 2.98x |
| parallel-fork us | 8 | 714.3 | 653.1 | 7.15x | 6.50x |
| parallel-fault ns | 8 | 1474 | 1329 | 5.82x | 5.55x |
| parallel-fault ns | 16 | 1583 | 1533 | 4.49x | 4.62x |

Every point is within its spread and none is flagged.  Without host KPTI
the kept changes are neutral, which is what they were expected to be.

With host KPTI (2 vCPUs, 3 reps): syscall 2,792 -> 213 ns, context-switch
51,180 -> 7,940 ns, fork-exec 3,271 -> 2,250 us, parallel-fork 1,533 ->
947 us, parallel-fault 3,098 -> 2,389 ns.  See Phase 7.

## Exit counts (whole perf suite, 2 vCPUs, counting builds, boot included)

| exit reason | before (tip) | after, no KPTI | before, KPTI | after, KPTI |
|---|---:|---:|---:|---:|
| total | 783,764 | 758,981 | 6,139,099 | 728,500 |
| PF | 506,853 | 502,890 | 500,044 | 500,273 |
| HC_TLB_INVLPG | 46,883 | 47,329 | — | 46,622 |
| HC_LOAD_PGTBL | 2,289 | 2,087 | 1,550 | 2,036 |
| SMOD->UMOD fallback (ERETU) | 1,816 | 1,660 | 2,780,825 | 1,571 |
| UMOD->SMOD fallback (SYSCALL) | 0 | 0 | 2,593,156 | 0 |
| HC_WRMSR | 91,495 | 83,482 | | 68,037 |

## Mechanism counters (after, no KPTI)

- PGTBL served by the switcher: 104,228 paired + 471 unpaired against 2,087
  exits (98% hit rate); 1,375 of the misses asked for a TLB flush.
- PGTBL publications / VM entries: 758,981 / 758,981 (every entry, by design).
- average range invalidation size: n/a (rejected; 1.13 pages when measured).
- direct switch success: 2,572,005 to supervisor with 0 fallbacks,
  2,756,734 to user with 1,660 fallbacks (99.94%).
- WRPKRU / syscall: 2.07 (5,328,739 / 2,572,005).
- PVCS dirty mark rate: one per exit, unchanged (Phase 9 not undertaken).

## Conclusions

### Proven wins
- Direct switching under host KPTI: 13x on syscall, 6.4x on context switch,
  8.4x fewer exits, KPTI-on now within noise of KPTI-off.
- insmod WARNs gone, two of them live NULL static calls.

### Neutral changes (kept for correctness or structure)
- NO_DS_CR3 fix: +1 exit per unpaired->paired switch removed, too rare in the
  suite to show on the matrix.
- Fused prev_roots[] scan; pkey out of make_spte() (TDP MMU back to upstream).
- #GP on a refused LOAD_PGTBL.

### Reverted ideas
- HC_TLB_INVLPG range batching: the targeted exits are guest boot.
- PKRU equality short circuit: never true for Linux.
- disallowed_va relocation: would narrow M7's coverage for a patched
  `xor %eax,%eax`.

### Remaining dominant cost
- Shadow paging: 66% of exits are page faults, 2 per first-touched page.
- HC_WRMSR (x2APIC ICR 57k, TSC_DEADLINE 26k) at 11%, the largest non-PF
  reason.

### Next recommended experiment
- An exit fastpath for PVM's HC_WRMSR on ICR and TSC_DEADLINE, as KVM has for
  VMX -- then the shadow MMU fault path itself, which the plan's decision tree
  (Case B) points at.
- Decide whether supervisor mode may run on the user PKRU (Phase 5).
