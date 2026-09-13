# PVM reflected-NP fault experiment — measurement log

Follows `docs/pvm-tuning-v3.md`.  Previous passes: `docs/TUNING-LOG.md`,
`docs/TUNING-LOG-v2.md`.

**Outcome: stopped at the Phase 1 gate.  Nothing is added to
`pvm-7.3-series`.**  The removable path is large (22–30% of vCPU time in
the fault benchmarks), but no not-present marker can both stay coherent and
be hit: a marker the guest can outrun is stale on 100% of reads, and a
marker that cannot be outrun is hit on 0.00–0.02% of reflected faults.  The
experiment is on branch `pvm-np-marker-experiment`.

## Commit

| | |
|---|---|
| series under test | `cd72210cfe08` (`pvm-7.3-series`, unchanged by this pass) |
| instrumentation / simulation | `pvm-np-marker-experiment`: `03461f4d86e1`, `d31c8fe7b8db`, `52f0ba6401ca`, `7abf563373c8` — all under `CONFIG_KVM_PVM_STATS`, none for upstream |
| testbed | `ddcf82f` (THP in L1), `f248747` (fault-rounds, marks, THP baseline), this log |

## Environment

| | |
|---|---|
| host CPU | i9-13900HK, 20 threads, no LA57 |
| L1 (PVM host) | 8 vCPUs, 8G, KPTI off, 4-level, **THP `[always]`** |
| guest | 1G, 1/2/4/8/16 vCPUs (16 is overcommitted) |
| repetitions | 5, medians (kvm-intel at 16 vCPUs: 4, the run was cut short) |
| LA57 | not run: no MMU change is kept (see Correctness) |

## P0 — representative baseline

`baselines/v3-thp-cd72210cfe08.tsv` (THP) and
`baselines/v3-nothp-cd72210cfe08.tsv` (control, same commit,
`CONFIG_TRANSPARENT_HUGEPAGE=n`).  Logs in `out/logs/v3-thp`,
`out/logs/v3-nothp`.

| benchmark | vCPUs | PVM THP | KVM THP | ratio | PVM no-THP | KVM no-THP | ratio |
|---|---:|---:|---:|---:|---:|---:|---:|
| page-fault ns/fault | 1 | 4435 | 1008 | 4.40x | 7028 | 9038 | 0.78x |
| page-fault ns/fault | 8 | 4359 | 1010 | 4.32x | 6939 | 9980 | 0.70x |
| parallel-fault ns/page | 1 | 4362 | 692 | 6.30x | 4474 | 734 | 6.09x |
| parallel-fault ns/page | 8 | 1567 | 215 | 7.27x | 1643 | 238 | 6.90x |
| fork-exec us | 1 | 1658 | 402 | 4.12x | 1738 | 461 | 3.77x |
| parallel-fork us | 8 | 625 | 88 | 7.09x | 647 | 103 | 6.28x |

Without THP in L1, kvm-intel's page-fault was *slower* than PVM's (9 µs):
the guest's 1G of RAM was 4K in EPT, and page-fault is the one benchmark
whose memory is cold.  With THP, EPT maps it 2M and kvm-intel drops 9x, so
the old "PVM wins page-fault" was an artefact of the environment.  THP also
cuts PVM's page-fault by 37% (fewer supervisor shadow faults behind each
guest fault).  parallel-fault barely moves in either vendor: it recycles the
same guest memory, so L1's page size is not what it pays for.  Every
conclusion below is against the THP baseline.

## P0 — reflected #PF classification

Counting host (`CONFIG_KVM_PVM_STATS=y`, THP), per-case marks, logs
`out/logs/l1-perf-pvm-v3count-*`.  `scripts/pf-classify.sh v3count`
reproduces the table.

Per page (all vCPUs):

| case | vCPUs | PF exits | reflected NP | shadow fixed | protection | reserved / PKU |
|---|---:|---:|---:|---:|---:|---:|
| page-fault | 1 | 2.009 | 1.000 | 1.006 | ~0 | 0 |
| parallel-fault | 1 | 2.006 | 1.000 | 1.003 | ~0 | 0 |
| fault-scaling | 1 | 2.009 | 1.001 | 1.005 | ~0 | 0 |
| parallel-fault | 4 | 2.067 | 1.000 | 1.063 | ~0 | 0 |
| page-fault | 8 | 2.013 | 1.000 | 1.011 | ~0 | 0 |
| parallel-fault | 8 | 2.026 | 1.000 | 1.022 | ~0 | 0 |
| fault-scaling | 8 | 2.009 | 1.000 | 1.006 | ~0 | 0 |

The reflected not-present faults are uniform:

- **mode**: 99.6% from UMOD (`pf_exit_umod` 131108 vs `pf_exit_smod` 537 in
  page-fault at 1 vCPU); all reflected NP carry USER.
- **access**: write ~100% (the benchmarks write-touch); fetch 0.
- **level**: 99.8% the 4K PTE (`reflect_np_l1`); the rest the PMD — one per
  new page table, 8 per fault-rounds round at 1 vCPU.
- protection faults: 1–5 per case; reserved and PKU: 0.

Every page costs the same two exits: the touch finds the guest PTE absent
and is reflected; the guest fills the PTE and returns; the retry finds a
present gPTE with no SPTE and is fixed.  TDP pays neither.

### Round by round

`perf/fault-rounds` (6 barrier-synchronised rounds, mmap + touch + munmap
of 16 MB per worker, marks around each round):

| vCPUs | | r1 | r2 | r3 | r4 | r5 | r6 |
|---|---|---:|---:|---:|---:|---:|---:|
| 1 | reflected NP | 4097 | 4096 | 4097 | 4096 | 4096 | 4096 |
| 1 | fixed | 4123 | 4107 | 4112 | 4104 | 4106 | 4104 |
| 1 | ns/page | 5210 | 4968 | 4438 | 4660 | 4612 | 4544 |
| 8 | ns/page | 1321 | 1435 | 1488 | 1864 | 1412 | 1548 |

Rounds 2..N are round 1 again, which answers the plan's "important
question": yes, every later round is *unmap clears the PTE → next access
finds it absent → reflect → guest fills it → fix*.  Nothing amortises
across rounds.  The per-page PMD level count (8 per round) shows the guest
page tables themselves survive munmap and are reused.

### Wall-time ceiling

Host time from exit to the next entry, by exit class (`exit_ns_*`, summed
over vCPUs), against wall time per page × vCPUs:

| case | vCPUs | host ns / reflected NP | host ns / fixed | lock wait ns/page | vCPU ns/page | reflected share | fixed share |
|---|---:|---:|---:|---:|---:|---:|---:|
| page-fault | 1 | 1316 | 1805 | 0 | 4566 | 29% | 40% |
| parallel-fault | 1 | 1350 | 1472 | 0 | 4479 | 30% | 33% |
| fault-scaling | 1 | 1688 | 1952 | 0 | 5576 | 30% | 35% |
| parallel-fault | 2 | 1521 | 1710 | 27 | 5279 | 29% | 33% |
| parallel-fault | 4 | 1618 | 2462 | 755 | 7440 | 22% | 35% |
| parallel-fault | 8 | 4747 | 6461 | 2871 | 19118 | 25% | 35% |
| fault-scaling | 8 | 2343 | 2913 | 343 | 9214 | 25% | 32% |

That host time does not include the hardware exception and the switcher,
so the reflected exit is at least 22–30% of vCPU time.  **The ceiling gate
(≥ 20%) is met** — for removing the whole round trip (Phase 2/3).  A
host-side fast path (Phase 1) removes only the guest walk inside those
1.3–4.7 µs.

## P1 — marker

### SPTE encoding audit

Candidate encoding: `SHADOW_NONPRESENT_VALUE | BIT_ULL(56)`, leaf 4K only.

| collision | why it cannot happen |
|---|---|
| hardware-present | bit 0 clear: the CPU faults with P=0 as today |
| `is_shadow_present_pte()` | `SPTE_MMU_PRESENT_MASK` (bit 11) clear: rmap, A/D, zap and flush code skip it |
| MMIO SPTE | shadow (non-EPT) MMIO value includes `PT_PRESENT_MASK`; the marker has bit 0 clear |
| `FROZEN_SPTE` | `BIT(63) \| 0x1a0`; the marker has none of 0x1a0 and has bit 56 |
| access tracking | `shadow_acc_track_mask` is 0 without EPT; saved bits (52/54) are only read from present SPTEs |
| EPT writable bits 53/55, TDP A/D 60–61 | EPT/TDP only, never in a PVM shadow table |
| `kvm_sync_spte()` | only skips `== SHADOW_NONPRESENT_VALUE`: a marker reaching `FNAME(sync_spte)()` would be treated as a present SPTE and its "pfn" mapped — **must be special-cased** (done in `d31c8fe7b8db`) |
| `mmu_page_zap_pte()` | leaves non-present non-MMIO SPTEs untouched: a trapped gPTE write would leave the marker behind — **must be special-cased** (`52f0ba6401ca`) |
| 4/5-level | leaf only; the marker never sits in a non-leaf slot, so M4's shared-PML5[0] handling is not involved |

The encoding is safe given those two special cases.  The problem is not
the bits.

### Coherence

The required invariant is *guest PTE NP → present ⇒ marker gone before the
guest can use it*.  The shadow MMU lets a guest write a last-level page
table without a trap once the page is **unsync**
(`mmu_try_to_unsync_pages()` → `kvm_unsync_page()`); from then on KVM learns
of the write only at the next sync (CR3 load, flush, INVLPG) — or, for an
NP → present change, from the fault that finds the entry present.  Source
answers to the plan's questions:

- *Are guest PT pages write-protected while shadowed?* Only while synced.
  The first write after a sync makes the page unsync and writable.
- *Does every PTE update trap?* No: every update to an unsync page is
  free, and that is the steady state of a page table being filled.
- *Which path gets trapped writes?* The write-protect fault →
  `kvm_mmu_track_write()` (emulated) or the unsync transition.  Neither
  looks at non-present SPTEs.

Two simulations measured it.  Neither acts on the marker: the fault is
handled as today and only the walk's verdict on a marker it would have
trusted is counted.

**Simulation 1** (`d31c8fe7b8db` + `52f0ba6401ca`): install at the reflected
walk, drop on sync and on a trapped write.

| case | vCPUs | installed | seen again | hit | stale |
|---|---:|---:|---:|---:|---:|
| page-fault | 1 | 65418 | 65418 | 0 | 65418 |
| parallel-fault | 1 | 24536 | 24536 | 0 | 24536 |
| fault-scaling | 1 | 32732 | 32732 | 0 | 32732 |
| parallel-fault | 8 | — | 195859 | 3 | 195856 |
| fault-scaling | 8 | — | 261697 | 1 | 261696 |

Every marker is read exactly once: by the retry, after the guest filled the
PTE through its unsync page table.  **100% stale.**  Trusting it would give
the guest a P=0 fault on a present PTE, forever.  This is the plan's stop
condition "a guest PTE can become present without trapping/updating marker
state", observed rather than argued.

**Simulation 2** (`7abf563373c8`): the only coherent variant.  Markers live
only in synced (write-protected) shadow pages: installed when
`mmu_sync_children()` — which write-protects and unlinks the page before
syncing it — finds a gPTE gone (not on the INVLPG sync, which leaves the
page unsync), never installed into an unsync page, and every marker in a
page dropped as it goes unsync.  Logs `out/logs/l1-perf-pvm-v3sync-*`.

| case | vCPUs | reflected NP | installed (walk / sync) | skipped: page unsync | cleared on unsync | hit | stale |
|---|---:|---:|---:|---:|---:|---:|---:|
| page-fault | 1 | 65563 | 128 / 58068 | 65306 | 1108 | 0 | 0 |
| parallel-fault | 1 | 24584 | 47 / 0 | 24489 | 1071 | 0 | 0 |
| fault-rounds | 1 | 24625 | 55 / 0 | 24515 | 55 | 0 | 0 |
| page-fault | 8 | 65550 | 131 / 58068 | 65291 | 1765 | 0 | 0 |
| parallel-fault | 8 | 196684 | 1293 / 22909 | 194995 | 23584 | 43 | 0 |
| fault-rounds | 8 | 196777 | 760 / 15933 | 195619 | 16685 | 5 | 0 |

Coherent — 0 stale — and useless: **hit rate 0.00–0.02%**.  99.1–99.6% of
reflected NP faults land in a page table that is already unsync, and the
markers a sync did leave are wiped by the guest's first write to that page,
before the rest of its entries are touched.

The one way to keep them is to refuse unsync for a page holding markers:
then each guest PTE fill is a write-protect exit plus emulation
(`kvm_mmu_track_write()`), one per page — the same count of exits as the
reflected fault it would replace, and a costlier one.  A marker per entry
needs a trap per write; that trap *is* the round trip being removed.

## Marker counters (template)

| | simulation 1 | simulation 2 |
|---|---|---|
| installed | ≈ reflected NP | ~1% of reflected NP (walk) + sync |
| hits | 0–3 | 0–43 |
| misses (not seen) | 0 | ≈ all |
| stale | 100% of seen | 0 |
| race fallback | n/a | n/a |

## Correctness

No change is kept, so the matrix was not rerun.  The simulation builds are
counting builds and booted every marked run cleanly (4-level, KPTI off,
1–8 vCPUs, 16 boots); their logs show no WARNING, BUG or Oops.

## Performance

No host fast path was built, so there is no A/B/A: the plan's Phase 1 gate
("if marker hit rate is low: stop") is decided by the hit rate before any
fast path would exist.

## Decision

**Stop.  Revert (nothing to revert on the series).**

## Explanation

- The reflected-NP exit is real and large: exactly one per page in every
  fault benchmark, 22–30% of vCPU time, identical round after round.
- It cannot be answered from shadow state, because shadow paging does not
  know the guest PTE is absent at the moment of the touch: the page table
  holding it is unsync, which is precisely what lets the guest fill it
  without a second exit.  Knowing, and keeping the knowledge true, requires
  write-protecting the page table, which costs one exit per fill — the one
  being saved.
- So Phase 2/3 (switcher reflection) have nothing coherent to act on; the
  security review of M1/M3/M4/M7/S9 is moot because no mapping, root or
  switcher path changed.

What would attack the same cost is not a marker but a different contract:
the guest telling the hypervisor it is about to populate a range (a
paravirt "prefault" / batched PTE-set hypercall, or letting the PVM guest
kernel install the SPTE as part of its own fault handling).  That is an
ABI change outside this plan.

## State left behind

- `pvm-7.3-series` tip `cd72210cfe08`, unchanged.
- `pvm-np-marker-experiment`: classification counters and both
  simulations, on top of `cd72210cfe08`.  The classification commit
  (`03461f4d86e1`) is reusable debug instrumentation.
- Host images rebuilt from `pvm-7.3-series`, THP on, no counters.
- Nothing pushed.
