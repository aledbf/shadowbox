# PVM direct PF optimization result — measurement log

Follows `docs/pvm-tuning-v4.md`.  Previous: `docs/TUNING-LOG-v3.md`.

## Environment

| | |
|---|---|
| commit | `pvm-direct-pf-experiment` on `cd72210cfe08` (`pvm-7.3-series` unchanged) |
| THP | on in L1 |
| KPTI | off (host "Not affected") |
| LA57 | not run yet (Phase A touches no paging code) |
| vCPUs | 1 and 8 (L1 has 8; 8 is noisy on this L0) |
| repetitions | 5, A/B order alternating per repetition, same host binary, only `direct_pf=` differs (`scripts/modarg-ab.sh`) |

Branch commits:

| commit | what |
|---|---|
| `d7216598db68` | reflected-#PF classification counters (debug, from v3) |
| `380bef4e3265` | `pvm_direct_pf_candidate()` + shadow-comparison counters |
| `e1fd480bf443` | `direct_pf=1`: deliver candidates without the MMU |
| (HEAD) | repeat rule keyed on the page (see "The P=0 ambiguity") |

## Baseline

From `TUNING-LOG-v3.md` (same commit, THP): per page 2.0 user #PF exits
= 1.0 reflected NP + 1.0 fixed; host ns/reflected 1316–1688 (1 vCPU);
page-fault 4435 ns, parallel-fault 4362 ns/page (1 vCPU, perf matrix).

## The P=0 ambiguity (why Phase A cannot be walk-free *and* identical)

The hardware #PF for "guest PTE absent" and for "guest PTE present, SPTE
absent" is the same: user, P=0, W as accessed.  Both are ~1 per page.
Without the walk nothing tells them apart, so a walk-free delivery must
accept that some deliveries are spurious (the guest gets a P=0 fault on a
mapping it has) and must never livelock on a shadow miss the guest cannot
fix.

Rule implemented (`pvm_direct_pf_candidate()`):

- user mode, error code without P/RSVD/PK/FETCH, lower-half CR2 (M7),
  no exception pending/injected, no async-#PF flags;
- not on the page of the previous candidate (so the retry after a
  delivery always reaches the MMU);
- at most 4 candidates in a row (so at least 1 user #PF in 5 is handled
  exactly as today, whatever the interleaving).

The first version ("never two candidates in a row") locked into the wrong
phase after one unpaired fault: page-fault counted 65577 spurious and 1
correct.  Kept as a commit; the page rule replaced it.

## Phase A — host-side direct PF

Shadow comparison (`direct_pf=0`, counting build: every candidate still
walks, and the walk's verdict is compared with what delivery would have
done):

| case | vCPUs | reflected NP | candidates | walk: NP (correct) | walk: fixed (spurious) | errcode diff | CR2 diff |
|---|---:|---:|---:|---:|---:|---:|---:|
| page-fault | 1 | 65546 | 65549 | 65545 | 4 | 0 | 0 |
| parallel-fault | 1 | 24586 | 24610 | 24586 | 24 | 0 | 0 |
| fault-scaling | 1 | 32810 | 32855 | 32808 | 47 | 0 | 0 |
| fault-rounds | 1 | 24599 | 24653 | 24592 | 61 | 0 | 0 |
| page-fault | 8 | 65551 | 65557 | 65551 | 6 | 0 | 0 |
| parallel-fault | 8 | 196651 | 196686 | 196645 | 41 | 0 | 0 |
| fault-scaling | 8 | 262285 | 262342 | 262275 | 67 | 0 | 0 |
| fault-rounds | 8 | 196731 | 196772 | 196716 | 56 | 0 | 0 |

candidate: 99.97–100% of reflected NP; spurious ≤ 0.25% of candidates;
guest-visible error code and CR2 identical bit for bit on every correct
delivery.  Fallbacks: present 1–17, fetch 1, event 0, run 0–7 per case.

Delivery (`direct_pf=1`, counting build):

| case | vCPUs | direct | slow reflected NP left | host ns/reflected (walk) | host ns/direct |
|---|---:|---:|---:|---:|---:|
| page-fault | 1 | 65580 | 0 | 1365 | 1175 |
| parallel-fault | 1 | 24589 | 0 | 1374 | 1245 |
| fault-scaling | 1 | 32866 | 3 | 1329 | 1309 |
| fault-rounds | 1 | 24646 | 1 | 1344 | 1487 |
| parallel-fault | 8 | 196669 | 2 | 1860 | 1736 |
| fault-scaling | 8 | 262311 | 9 | 2175 | 1759 |

Hardware user #PF exits per page unchanged (2.0), as the plan expected.
The guest walk and the MMU entry are **~100–200 ns** of the ~1.3 µs a
reflected exit costs; the rest is the round trip itself.

Timing A/B (`out/perf/ab-direct-pf.tsv`, timing build, medians of 5):

| benchmark | vCPUs | A direct_pf=0 | B direct_pf=1 | delta | ranges |
|---|---:|---:|---:|---:|---|
| page-fault ns/fault | 1 | 4485 | 4297 | **−4.2%** | 4439–4573 vs 4269–4396, disjoint |
| parallel-fault ns/page | 1 | 4268 | 4069 | **−4.7%** | 4222–4338 vs 4051–4117, disjoint |
| fault-scaling ns/page | 1 | 4185 | 4166 | −0.5% | overlapping |
| fault-rounds r2..r6 ns/page | 1 | 4173–4317 | 4063–4340 | −4%..0% | overlapping |
| syscall ns | 1 | 212 | 209 | −1.3% | noise |
| context-switch ns | 1 | 7726 | 7901 | +2.3% | noise |
| fork-exec us | 1 | 1643 | 1660 | +1.0% | noise |
| any | 8 | | | −35%..+50% | L0 noise (single reps 2–8x), no signal |

Correctness with `direct_pf=1`: default 38/38 (includes the new
`mm/fault-race`), security 22/22, perf and scaling cases clean, no WARN.

Decision: **gate passed on its own terms** (hit rate ~100%, no error-code
or CR2 difference, host time per reflected fault −10–15%, page-fault and
parallel-fault −4% outside noise), with one qualification the plan did
not foresee: guest-visible behaviour is identical *except* for ≤ 0.25%
spurious not-present faults on present mappings, which a walk-free design
cannot avoid.  Linux handles them (fault on a present PTE returns), but
they count as minor faults and require the guest's consent — i.e. a
feature bit, if this ever leaves the experiment.

What Phase A proves for Phase B: the classifier is right and cheap; the
remaining ~1.2 µs per page is the exit/entry, which only an in-switcher
delivery removes.

## GSBASE

Raw cost, L0, one P-core, 3 × 50M iterations: RDGSBASE 3.4–3.5 ns,
WRGSBASE 6.2–6.3 ns.  A getpid round trip does RD+WR (UMOD→SMOD) and WR
(SMOD→UMOD): ~16 ns of 213 ns = **7.5%**.  (L1 runs these without exits;
same order.)

Ownership while a vCPU runs:

| state | hw GSBASE | hw KERNEL_GS_BASE | elsewhere |
|---|---|---|---|
| outside vCPU / host restored | host per-CPU | qemu `thread.gsbase` | guest current-mode GS in `segments[GS].base`; guest kernel GS in `msr_kernel_gs_base` |
| state loaded, in hypervisor | host per-CPU | guest current-mode GS | other mode: PVCS `user_gsbase` or `msr_kernel_gs_base` |
| guest UMOD | guest user GS | host per-CPU | guest kernel GS: `TSS_extra.smod_gsbase` |
| guest SMOD | guest kernel GS | host per-CPU | guest user GS: PVCS `user_gsbase` |
| entry (SYSCALL/exception) after swapgs | host per-CPU | guest current-mode GS | — |

Every entry needs the host per-CPU base in KERNEL_GS_BASE to reach
`TSS_extra` after `swapgs`.  While the guest runs there are three live
values (host, guest current, guest other) and two hardware slots, so a
mode switch must write GSBASE once and (UMOD→SMOD) read the guest user
value it cannot otherwise know — the guest writes it at CPL3 without
trapping.  "swapgs only" needs an entry that finds per-CPU data without
GS (per-CPU entry text or RDPID lookup), a redesign of every switcher
entry for at most 7.5% of a syscall.  **Stopped** under "state ownership
becomes more complex than current path".
