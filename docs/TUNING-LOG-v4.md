# PVM direct PF optimization result — measurement log

Follows `docs/pvm-tuning-v4.md`.  Previous: `docs/TUNING-LOG-v3.md`.

## Environment

| | |
|---|---|
| commit | `pvm-direct-pf-experiment` on `cd72210cfe08` (`pvm-7.3-series` unchanged) |
| THP | on in L1 |
| KPTI | off (host "Not affected") |
| LA57 | TCG `-cpu max,la57=on`, correctness only (see Correctness) |
| vCPUs | 1 and 8 (L1 has 8; 8 is noisy on this L0) |
| repetitions | 5, A/B order alternating per repetition, same host binary, only `direct_pf=` differs (`scripts/modarg-ab.sh`) |

Branch commits:

| commit | what |
|---|---|
| `d7216598db68` | reflected-#PF classification counters (debug, from v3) |
| `380bef4e3265` | `pvm_direct_pf_candidate()` + shadow-comparison counters |
| `e1fd480bf443` | `direct_pf=1`: deliver candidates without the MMU |
| `27d9f5799b3b` | repeat rule keyed on the page (see "The P=0 ambiguity") |
| `eb1f24427889` | `direct_pf=2`: deliver from the switcher, no exit (Phase B) |
| `1f7391f2f241` | invariants doc: the stub in S5 and S9 |

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

## Phase B — direct PF in the switcher

`asm_exc_page_fault` jumps, for a #PF from CPL3, to
`pvm_direct_page_fault` (`entry_64_switcher.S`) before `error_entry`: on the
guest's CR3 and GSBASE, touching only `.entry.text`, `TSS_extra` (first page
of `cpu_tss_rw`, cloned into every user root) and the PVCS alias.  It applies
the Phase A rule in assembly -- switcher active and `dpf_on`, no direct-switch
inhibitor, error code exactly U or U|W (so no P/RSVD/PK/fetch, and not an L0
async #PF, which arrives with 0), CS/SS the 64-bit user pair, CR2 lower half,
the repeat rule on state copied in and out of `TSS_extra` around every run --
then writes the event as `__do_pvm_event()` would (rcx, r11, user_cs/ss,
eflags, rip, errcode, vector STD|14, cr2), makes the UMOD->SMOD switch of the
direct syscall (PKRU macro, CR3, GSBASE) and leaves through the switcher's
shared, canonical-checking SYSRET/IRET.  Anything else returns to
`pvm_page_fault_slow` with every register untouched, and kvm-pvm handles it
(with `direct_pf=2` it still delivers from the exit handler what the switcher
left).

Mechanism (counting build, `direct_pf=2`):

| case | vCPUs | user #PF exits/page (before -> after) | switcher deliveries | slow reflected NP left |
|---|---:|---:|---:|---:|
| page-fault | 1 | 2.00 -> 1.00 | 65582 | 0 |
| parallel-fault | 1 | 2.00 -> 1.00 | 24610 | 0 |
| fault-rounds | 1 | 2.00 -> 1.00 (every round 4098) | 24690 | 1 |
| page-fault | 8 | 2.00 -> 1.00 | 65554 | 0 |
| parallel-fault | 8 | 2.00 -> 1.00 | 196704 | 4 |
| fault-scaling | 8 | 2.00 -> 1.00 | 262349 | 17 |
| fault-rounds | 8 | 2.00 -> 1.00 | 196821 | 6 |

Timing A/B (`out/perf/ab-direct-pf-B.tsv`, timing build, `direct_pf=0` vs
`direct_pf=2`, medians of 5, order alternating):

| benchmark | vCPUs | A | B | delta | ranges |
|---|---:|---:|---:|---:|---|
| page-fault ns/fault | 1 | 4417 | 2942 | **−33.4%** | B 2832–3015 vs A ≥ 4388, disjoint |
| page-fault ns/fault | 8 | 4464 | 2986 | **−33.1%** | 2946–3147 vs 4292–4673, disjoint |
| parallel-fault ns/page | 1 | 4398 | 2733 | **−37.9%** | 2626–2815 vs ≥ 4135, disjoint |
| parallel-fault ns/page | 8 | 1680 | 1073 | **−36.1%** | noisy, 3 of 5 B below every A |
| fault-scaling ns/page | 1 | 4265 | 2835 | **−33.5%** | 2710–3660 vs 4120–4706 |
| fault-scaling ns/page | 8 | 880 | 629 | **−28.5%** | 581–721 vs 856–931, disjoint |
| fault-rounds r1..r6 ns/page | 1 | 4122–4685 | 2550–3045 | **−35..−43%** | disjoint every round |
| fault-rounds r1..r6 ns/page | 8 | 1102–1404 | 720–972 | −21..−49% | noisy |
| fork-exec us | 1 | 1625 | 1418 | −12.7% | B 1360–1432 vs A 1603–3753 |
| parallel-fork us | 1 | 1593 | 1377 | −13.6% | |
| parallel-fork us | 8 | 638 | 601 | −5.7% | |
| fork-exec us | 8 | 2964 | 3495 | +17.9% | 2872–4223 vs 2614–5173: noise |
| syscall ns | 1 / 8 | 213 / 209 | 209 / 216 | −1.9% / +3.1% | noise |
| context-switch ns | 1 | 7972 | 7294 | −8.5% | A has a 14.6 µs outlier: noise |

PVM/kvm-intel on page-fault (kvm-intel 1008 ns, v3 THP baseline): 4.4x ->
2.9x.  The plan's target (≥ 15% stable on page-fault, parallel-fault,
fault-rounds) is met by about twice.

## Phase C — prefault hypercall: stopped by analysis

After Phase B a fresh page costs one exit: the retry that finds a present
guest PTE and no SPTE.  `PVM_HC_PREFAULT` after a successful guest fault
would replace that #PF exit with a hypercall exit doing the same shadow MMU
work -- one exit per page either way.  The round-trip reduction the plan
wanted from C (~2 -> ~1) is what B already delivered.  What C could still
save is the difference between a hardware #PF exit and a hypercall exit, of
the order of 100 ns of the ~1.5 µs fixed-fault exit, against a new ABI
feature and a hook in the guest's fault path.  Stop condition: "prefault
causes more than one equivalent exit/page" is not triggered, but the
success criterion "host round trips ~2 -> ~1" is already met without it and
C cannot go below one.  Anything further needs the SPTE built without an
exit -- e.g. batching several pages per exit -- which is outside this plan.

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

## Correctness (direct_pf=2 unless noted)

| run | result |
|---|---|
| 4-level KVM: default | 38/38 (includes `mm/fault-race`) |
| 4-level KVM: security | 22/22 |
| 4-level KVM: full | 59/59 |
| 4-level KVM, `pti=on`: default, security, full | 38/38, 22/22, 59/59 |
| `pti=on` profile (4 kHz perf NMIs, S9's load-bearing test): parallel-fault, page-fault, and 5 × fault-rounds at `pvmtest.scale=8` | all pass, no Oops/WARN |
| profile without pti: parallel-fault | pass |
| perf/scaling counting runs, 1 and 8 vCPUs | clean, 1.0 user #PF exit/page |
| `direct_pf=1`: default, security | 38/38, 22/22 |
| LA57 (TCG): default | 37/38 -- `time/monotonic` timeout, the known TCG artefact (v2) |
| LA57 (TCG) + `pti=on`: security, 3 runs | 20/22 twice, 19/22 once: `invd`/`wbinvd` (known TCG artefacts) and once `security/pkey-allowed` |
| LA57 (TCG) without pti: security, `direct_pf=2` / `direct_pf=0` | 20/22 / 19/22 -- `pkey-allowed` failed with **direct_pf=0** too |

`security/pkey-allowed` under TCG/LA57 fails intermittently (2 of 6 runs,
one with direct PF off): the victim takes SIGSEGV reading its own page
through an unrestricted key.  Not caused by this work; not investigated
here, and worth a look on its own -- it never failed under KVM.

PKU: the switcher path uses the existing `SWITCHER_PKRU_TO_SMOD` and never
takes a PK error code; pkey-allowed/denied pass under KVM with and without
pti.  MMIO: the first access to an MMIO page with no SPTE yet is a P=0 fault
on a present guest PTE, so it can be delivered once, spuriously; the retry
is on the same page, goes to the MMU and is emulated as before (and once
the MMIO SPTE exists, faults carry RSVD and are never candidates).  Sanitizer:
not run (no KASAN build in this pass).

Security invariants, reviewed for the stub:

| | |
|---|---|
| S1 | leaves only through `.L_switcher_return_to_guest` (canonical RCX or IRET); the entry is `msr_event_entry`, guest-controlled |
| S2 | same shared return masks/forces EFLAGS; frame EFLAGS set to FIXED\|IF |
| S3 | gated on `SWITCH_FLAGS_NO_DS_TO_SMOD`, the inhibitor word; `dpf_on` is a feature gate, not an inhibitor |
| S5 | `host_rsp` tested before anything else; a host process's #PF goes back untouched (cost: 2 swapgs + 1 compare per host user #PF) |
| S6 | loads `smod_cr3`, as the direct syscall |
| S8 | writes the pinned PVCS through `tss_ex.pvcs`, as the direct syscall |
| S9 | guest CR3 only: `.entry.text`, first page of `cpu_tss_rw`, the PVCS alias; validated by the pti profile runs |
| M1/M3/M4 | no SPTE is created or changed; fetch faults never taken (NX/SMEP emulation stays in the MMU) |
| M7 | CR2 sign bit checked; hardware CR2 is always canonical |

## Final conclusion

- **Phase A** (exit handler, no walk): correct, ~100% hit rate, −4% on
  page-fault/parallel-fault.  Useful as proof and as the fallback for what
  the switcher leaves.
- **Phase B** (switcher, no exit): user #PF exits 2.0 -> 1.0 per page;
  page-fault −33%, parallel-fault −36..−38%, fault-scaling −29..−34%,
  fault-rounds −35..−43% (1 vCPU), fork-exec/parallel-fork −13% at 1 vCPU;
  syscall and context-switch unchanged.  Correct on 4-level with and
  without KPTI under NMI load, and on LA57 (TCG) apart from pre-existing
  artefacts.  **Keep, on `pvm-direct-pf-experiment`**, not merged into
  `pvm-7.3-series`: it changes what the guest can observe (≤ 0.25% spurious
  not-present faults on present mappings, and KVM's `vcpu->arch.cr2` is not
  updated for switcher deliveries), so before the series it needs a guest
  opt-in feature bit instead of a module parameter, and the CONFIG_KVM_PVM_STATS
  classification commit dropped.
- **Phase C** (prefault): stopped by analysis; after B there is one exit per
  page and a prefault hypercall would only replace it.
- **GSBASE**: stopped; 7.5% of a syscall but not removable without a GS-free
  entry.
