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

Side finding, not fixed yet: `handle_hc_load_pagetables()` drops the return
value of `kvm_set_cr3()`, so an invalid CR3 load is silently a no-op for the
guest.

Benchmark before/after: not yet run on a non-counting build (the real-guest
sequence is rare: 1,703 NO_DS_CR3 ERETU fallbacks in a whole suite, and not all
of them are this sequence).  Expected neutral on the matrix.

## Phase 2 — observability

Kernel `0b1e3c9040bf` (`CONFIG_KVM_PVM_STATS`, default n).  Build a counting
host with `HOST_CONFIG_EXTRA=CONFIG_KVM_PVM_STATS=y`; perf-matrix refuses one.
Every number in the Baseline counter tables above comes from it.

## Open items found on the way

- `HC_WRMSR` (ICR, TSC_DEADLINE): exit fastpath for PVM.
- `handle_hc_load_pagetables()` ignores `kvm_set_cr3()` failure.
- A guest without PCIDE sends LOAD_PGTBL with the TLB bit set on every
  context switch (`~val >> 63`), which the switcher never serves.  Linux on
  x86-64 with PCID hardware has PCIDE; worth a check on hosts without PCID.
