# PVM: where it stands, what it costs, what is left

For someone — or something — picking this up cold, with one of two jobs:

- **make it faster**, or
- **make it smaller**, meaning fewer changes to code shared with kernels that
  will never run a PVM guest.

Read "The commitment" and "Ruled out" before proposing anything. Most of the
obvious ideas in both directions have already been tried and measured, and the
reasons they failed are the useful part of this document.

Two repositories:

- `linux-aledbf`, branch **`pvm-7.3-series`** — the kernel series, 17 commits
  on `v7.3-rc2`. Each commit builds (`bzImage` + `modules`) on its own.
- `pvm-testbed` — the harness. Nothing here boots PVM on the development
  machine directly: L0 runs L1 as an ordinary KVM guest, L1 loads `kvm-pvm`
  and runs the guest inside it. Everything below is measured in that nesting.

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
  with `_PAGE_USER` stripped. That is what makes a CPL 3 guest possible at all,
  and it is why host KPTI needed its own work (invariant S9).

`Documentation/virt/kvm/x86/pvm-invariants.rst` in the kernel tree states the
properties this must hold and how each is checked. Read it before touching the
switcher or the shadow MMU hooks.

---

## Performance

### The matrix

`baselines/7.0.0-31-generic__13th-Gen-Intel-R-Core-TM-i9-13900HK.tsv`, medians
of 5 reps, PVM versus the same guest under `kvm-intel`:

| benchmark | 1 cpu | 2 | 8 | 16 |
|---|---|---|---|---|
| `perf/syscall` (getpid) | 3.33x | 3.29x | 3.32x | 3.25x |
| `perf/context-switch` (pipe round trip) | 1.61x | 1.95x | — | 1.53x |
| `perf/page-fault` (single-threaded) | 1.20x | 1.16x | 1.12x | 1.11x |
| `perf/fork-exec` | 5.12x | 5.02x | 3.72x | 2.70x |
| `perf/parallel-fork` | 5.34x | 5.15x | 7.83x | 6.40x |
| `perf/parallel-fault` | 10.24x | 6.67x | 22.46x | 8.48x |

> **This baseline predates the host-KPTI and protection-key work.** It was
> taken at `v7.3-rc2-50-g827a44dd7911`; the series tip is seven commits later.
> The one figure known to have moved is `perf/syscall`, from 200 ns to 240 ns
> (the protection-key swap). **Re-run `make perf-baseline` before drawing
> conclusions** — it is the first thing any performance work should do.

`parallel-fault` at 8 cpus is the worst number and also the noisiest
(min 3919, max 7322 over 4 reps). Do not chase it without more reps.

### What the syscall cost is made of

This is the one cost that has been decomposed, and the decomposition **refuted
the guess that preceded it**. Using the guest's own KPTI as a reference for
what a per-syscall CR3 switch costs on this machine:

| guest | ns/getpid | vs. KVM without guest KPTI |
|---|---|---|
| KVM, guest `pti=off` | 67.7 | 1.00x |
| KVM, guest `pti=on` | 174.0 | 2.57x |
| PVM | 240.2 | 3.55x |

So **most of PVM's syscall cost is the CR3 switch it cannot avoid.** A PVM
guest pays a KPTI-shaped syscall whether or not it enables KPTI, because the
two-root split is the only thing between guest user space and the guest kernel.
PVM's own event handling is the remaining ~1.38x, of which roughly 13 ns is the
protection-key swap.

The earlier reading of the same 3.3x — "the CR3 switch is 20-30%, so 2.5x is
switcher overhead waiting to be optimised" — was wrong by an order of
magnitude. There is perhaps 100 ns of addressable cost on that path, not 150.

### What has already been won

`perf/context-switch` went **3.53x → 1.61x** by letting the switcher serve
`PVM_HC_LOAD_PGTBL` without leaving guest mode. A guest context switch is a CR3
load, which is privileged, so it was a hypercall and therefore an exit: 74% of
all exits and 63% of the time spent handling them, for work the hypervisor did
by finding a root `prev_roots[]` already held. The hypervisor now publishes up
to four `(guest_cr3, smod_cr3, umod_cr3)` triples into `tss_ex.pgtbl` on every
entry and the switcher matches against them.

Two things that went wrong on the way, both worth knowing:

- The first version halved the exits and did not get faster, because taking the
  fast path set `SWITCH_FLAGS_NO_DS_CR3` and killed the mode direct switch:
  620k CR3 exits became 606k `ERETU` exits. Publishing **both** roots of the
  pair fixed it. Any new fast path has to keep the others alive.
- It crashed on a guard page until the missing `swapgs` was found. Three
  plausible hypotheses were wrong first; an experiment that loaded a known-good
  CR3 is what localised it.

---

## Ruled out, with reasons

Do not re-propose these without new evidence.

**Protection keys instead of the two-root split.** The attractive idea: one
shadow root for both guest modes, with PKRU denying the kernel's keys while in
guest user mode, so a guest syscall needs no CR3 load. Dead: `WRPKRU` is
unprivileged and does not trap, so guest user code simply unlocks the kernel's
keys. `CR4.PKE` cannot make it privileged and the hypervisor cannot intercept
it. This is the single biggest performance idea and it does not work.

**PKS for the same purpose.** PKS governs supervisor-mode accesses to `U=0`
pages. M1 makes every page `U=1`. Nothing to govern.

**Cloning the host's kernel PGD into the guest root under host KPTI.** Makes
everything work immediately — it was the experiment that proved the KPTI
diagnosis — and defeats KPTI for the guest, which is a CPL 3 attacker sharing
the address space. That is precisely the threat KPTI exists for.

**Sharing non-leaf shadow pages between the guest's two roots.** An attractive
memory saving; fuses the trees that `role.access & ACC_USER_MASK` separates and
defeats the NX-based SMEP emulation (M4, M3).

**Disabling PCID under host KPTI.** Measured, on the hypothesis that PVM's use
of CR3 bit 11 collided with KPTI's. It is not a collision — PVM's PCIDs start
at 8 and the host's dynamic ASIDs stop at 6 — and disabling it changed nothing.

**Tuning `tlb_single_page_flush_ceiling` in the guest.** Swept 1, 2, 4, 8, 33
against `parallel-fault`: 3718-4111 ns/page, no trend, and the exit counts
barely moved (PF ~147-156k, `HC_TLB_INVLPG` ~38k throughout). The cost is not
in the choice between one flush and many.

**A negotiated linear-address range** (`MSR_PVM_LINEAR_ADDRESS_RANGE`). Removed
in favour of "the guest owns the lower half", which is a sign test. The
arithmetic had already been wrong once. It also made PVM compatible with
`CONFIG_KASAN_VMALLOC`.

**PV MMU as an obvious win.** Research found KVM removed its own PV MMU because
it was *slower* than shadow paging. That was a different setup and is not a
closed door for PVM specifically — PVM has no EPT to fall back on — but anyone
proposing it should read why it failed there first.

---

## Open leads: performance

Ordered by how much is on the table.

1. **The shadow MMU.** `parallel-fault` 10-22x, `parallel-fork` 5-8x,
   `fork-exec` 2.7-5.1x. This is where nearly all of the remaining cost is, and
   almost nothing has been tried. Shadow paging costs exactly 2.0 exits per
   first-touched page. At the tip, `parallel-fault`'s exits are page faults
   and essentially nothing else.

2. **`HC_WRMSR`: IPIs and the timer.** The largest exit reason after page
   faults, 11.7% over the perf suite: 67k x2APIC ICR writes and 24k
   TSC_DEADLINE writes. KVM serves both in its exit fastpath for VMX; PVM
   always returns `EXIT_FASTPATH_NONE`. See `docs/TUNING-LOG.md`.

   (`HC_TLB_INVLPG` batching was tried and rejected: 38k of the suite's 47k
   single-page flushes happen in the first 60ms of guest boot, remapping early
   fixmap slots, and the ranges benchmarks flush average 1.1 pages. The
   implementation is on kernel branch `pvm-tlb-range-experiment`.)

3. **More work served inside the switcher.** The CR3 fast path is the proof
   that this pays. What else exits for work the hypervisor does by lookup?
   Get the histogram (`make exits`, then `pvmtest resolve-exits`) and look
   at the top reasons with their guest RIPs.

4. **The ~1.38x of PVM's syscall over a KPTI syscall.** Not structural, but
   small in absolute terms: ~66 ns. The switcher does ~6 PVCS stores, a
   `swapgs` pair, `rdgsbase`/`wrgsbase`, and the guest then runs its own entry
   sequence — two entry paths per syscall where native has one.

5. **The protection-key swap, ~13 ns of that.** Currently unconditional for any
   guest with `CR4.PKE`, which a Linux guest always sets. A "user PKRU equals
   the supervisor value" short circuit never fires for Linux, whose tasks run
   on `init_pkru` = 0x55555554 (counted: every one of 2.6M swaps). Making it
   free means letting supervisor mode run on the user's PKRU, which changes
   what the guest kernel's accesses to user pages are subject to. A decision,
   not an optimisation; see `docs/TUNING-LOG.md`, Phase 5.

6. **`XCR0` switching.** PVM deliberately keeps the guest's `XCR0` equal to the
   host's to avoid an `XSETBV` on every entry and exit, which is expensive when
   the PVM host is itself a guest because writing `XCR0` exits to L0. The size
   of that cost has never been resolved from run-to-run drift.

---

## Open leads: fewer changes

Current footprint of the series, `v7.3-rc2..pvm-7.3-series`, 113 files,
10065 insertions, 298 deletions:

| area | files | + | − | note |
|---|---|---|---|---|
| `arch/x86/kvm/pvm` | 4 | 4123 | 0 | new, nothing shared |
| `arch/x86/kernel` | 18 | 1225 | 32 | mostly PIE + the guest side |
| `arch/x86/entry` | 9 | 1056 | 21 | switcher (new) + hooks |
| `arch/x86/include` | 26 | 944 | 69 | |
| `tools` | 8 | 214 | 13 | objtool, perf |
| `arch/x86/kvm/mmu` | 5 | 211 | 33 | the shared shadow MMU |
| `arch/x86/mm` | 9 | 94 | 30 | |
| `include`, `mm`, `scripts` | 6 | 26 | 0 | |

29 files outside `arch/x86/kvm/pvm/` mention a PVM hook.

Targets, in the order a reviewer would care:

1. **The PIE kernel is the biggest single chunk** — 45 files, 815 insertions,
   and the only commit with meaningful deletions. It exists because a PVM guest
   lives in the lower half and a `-mcmodel=kernel` image cannot: it is linked
   for the top 2GB and says so in every absolute relocation. Ask whether the
   whole of `CONFIG_X86_PIE` is needed, or whether a smaller "relocatable to
   the lower half" change would do. Note it is genuinely independent of PVM and
   arguably wants its own upstream life.

2. **The shared shadow MMU** — 211 lines across 5 files, and the part upstream
   will scrutinise hardest. Three globals (`shadow_force_user_mask`,
   `shadow_emulate_smep_with_nx`, `shadow_pkey_mask`) that stay zero for every
   other vendor, one new parameter threaded through `make_spte()` and
   `mmu_set_spte()` for the protection key, and
   `mmu_adjust_kernel_only_access()`. Ask whether the pkey parameter could be
   carried differently, and whether the three globals want to be one.

3. **The entry hooks** — three tests of `TSS_extra(host_rsp)` in `entry_64.S`
   plus the CR3 macros in `calling.h`. All inside `CONFIG_X86_PVM_SWITCHER`,
   and one compare against a per-CPU zero when built in with no guest running.
   `SWITCHER_PARANOID_ENTER_CR3` adds an `SGDT` per IST entry under an
   `ALTERNATIVE` on `X86_FEATURE_PTI`. Probably already near-minimal; check.

4. **`arch/x86/mm/pti.c`, `xen/`, `power/`, `platform/pvh/`** — small, and all
   consequences of PIE rather than of PVM.

The invariants document also records what was *removed* by earlier
simplification, which is the shape of a successful footprint reduction: moving
the guest kernel into the lower half retired invariant M7, the range MSR, four
per-vCPU bounds, the `get_vm_area_align()` reservation, the PML bookkeeping and
the LA57 top-p4d merge in one move.

---

## What is missing functionally

- **PKS, and therefore SMAP for the guest.** Protection keys work for guest
  user space; supervisor mode runs on `PKRU` 0, every key permitted, rather
  than on the guest's `MSR_IA32_PKRS`. So a guest kernel has no SMAP and no
  substitute. The hypervisor side of the split exists; what is left is the MSR
  and the guest distributing keys. See "Protection Keys" in `pvm-spec.rst`.
- **FRED hosts.** Refused at module load: the switcher owns the IDT entry
  paths and FRED replaces them.
- **`x86/userspace_msr_exit_test`** fails one subtest, and cannot pass: its
  guest is not a PVM guest, so the #GP KVM injects when userspace refuses a
  WRMSR has no event entry to go to and becomes a triple fault. Recorded in
  `configs/kvm-selftests-expect.txt`.
- **Not tested on a 5-level host**, the per-VM PVCS alias included.

---

## How to measure without fooling yourself

Every one of these cost real time at least once.

- **`pvmtest boot l1` does not build anything.** It boots whatever is in
  `out/`. Run `make host-kernel` (or a target that depends on it) after editing
  the kernel, or you will measure a module that does not contain your change.
- **Do not check a kernel build with `grep error:`.** `modpost` failures read
  `ERROR: modpost: symbol 'x' undefined!`, which that pattern misses. A module
  silently stayed stale for several measurements that way. Check the exit
  status, or `grep -iE '^ERROR|error:'`.
- **Metric lines must end with `#END`.** Interleaved `printk` corrupted a
  metric line once and produced a plausible wrong ratio (`0.00x`);
  the parser ignores metric lines without it.
- **Use `make perf-baseline` and compare against the recorded baseline**, not
  against a number in a chat log. `make perf-matrix` takes medians over
  reps; single runs on this machine drift by tens of percent.
- **Run things through a battery** (`batteries/*.pvm`, `make battery B=...`,
  `tools/pvmtest`), not a shell loop written for the occasion: it checks the
  host build the item needs, keeps every log with a manifest under
  `out/results/`, applies the known-failure lists, and reduces matrices and
  A/B runs the same way every time.
- **`pr_info` from the module does not reach L1's live console during a guest
  run; `pr_emerg` does.** A whole wrong conclusion — "`pvm_vcpu_run` is never
  called" — came from that.
- **`L1_QMP=<path>`** opens a QMP socket on L1's QEMU. `info registers -a` on a
  wedged L1 is how the KPTI livelock was found; guessing from a quiet console
  had failed for hours.
- **`SELFTESTS=<glob>`** stages only the KVM selftests you are asking about. A
  full sweep is twenty-odd minutes.
- **`pvmtest.guest_timeout=<s>`** shortens the wait for a hanging guest, whose
  serial log only reaches the console once its QEMU exits.

## The one ABI affordance worth knowing

`PVM_CPUID_SIGNATURE` / `PVM_CPUID_FEATURES` (0x40000200) now carry an ABI
version and a feature bitmap, and the guest refuses to run on a version it was
not built for. **Anything that needs a new hypercall, a new PVCS field, or an
optional behaviour can take a `PVM_FEATURE_` bit and does not need a version
bump.** Before that existed, every ABI change was a silent break — the removal
of `MSR_PVM_LINEAR_ADDRESS_RANGE` would have left an old guest reading zero and
relocating itself into the hypervisor's half. No feature bits are defined yet
(the one the TLB range experiment took is not in the series).
