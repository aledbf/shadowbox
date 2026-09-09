# Status

Written alongside the 7.3 port, before any of it had been booted.

## What has been run

Stage 0 passes, on both entry paths, on an i9-13900HK under ordinary KVM:

```
PVMTEST-RESULT: ok tag=stage0-pvh-smoke      suite=smoke   pass=11 fail=0
PVMTEST-RESULT: ok tag=stage0-bzimage-smoke  suite=smoke   pass=11 fail=0
PVMTEST-RESULT: ok tag=stage0-pvh-default    suite=default pass=31 fail=0
```

So the PIE kernel boots, reaches user space, and behaves like a kernel.
That covers `__startup_64()`, the moved page-table entries, the runtime
`kernel_map_base`, the relocated fixmap, and the PVH entry changes -- all
of which had never been executed before this.

Two results are worth reading rather than counting:

- `boot/entry-path` reports boot protocol **0x020c** on the ELF image,
  which is the version `init_pvh_bootparams()` stamps and nothing else
  does. The PVH entry really did run, which means the PIE
  `call xen_prepare_pvh` from the identity mapping works.
- `pvm/kernel-map` reports `_text = 0xffffffff81000000`. That is the
  right answer *here*: `pvm_detect()` returns false under plain KVM, so
  the relocation is correctly a no-op. Under a PVM host this address has
  to be somewhere else, and `pvm/relocated` says so once
  `pvmtest.expect=pvm` is passed.

**Stage 2 passes.** A PVM guest boots to user space and passes the
default suite under a PVM host:

```
PVMTEST-RESULT: ok tag=pvm-guest suite=smoke   pass=11 fail=0
PVMTEST-RESULT: ok tag=pvm-guest suite=default pass=31 fail=0
#   _text = 0xffffc2ff81000000
#   kernel was relocated out of the top 2GB: PVM early relocation ran
```

So the switcher works. The guest runs at hardware CPL3 under shadow
paging, and `pvm/relocated` -- which is a hard failure when the harness
says this is a PVM run -- confirms the kernel is outside the 2GB the
hypervisor withholds. `entry/ptrace-singlestep` passes too: 2000 steps,
which is the only case that exercises the switcher's SINGLE_STEP
inhibitor.

Stage 1 passes as a matter of course: the host kernel boots in L1,
`kvm-pvm` registers, and `/dev/kvm` appears. `kvm_x86_vendor_init()`
WARNs 15 times on the way for kvm_x86_ops the PVM backend does not
implement -- see below.

Stage 0 passes on both entry paths, so the same image is still an
ordinary kernel:

```
PVMTEST-RESULT: ok tag=regress-pvh-smoke      suite=smoke   pass=11 fail=0
PVMTEST-RESULT: ok tag=regress-bzimage-smoke  suite=smoke   pass=11 fail=0
PVMTEST-RESULT: ok tag=regress-pvh-default    suite=default pass=31 fail=0
```

`make full` passes too: 34 of 34, including the process-churn and
fault-storm cases.

`make perf`, on the same machine, same guest, same host kernel, same
nesting depth -- only the KVM vendor module differs between the last two
columns:

```
metric                                 KVM (L0)     KVM (L1)     PVM (L1)  PVM/KVM
perf/context-switch.ns_per_roundtrip        422.5        339.8        432.9    1.27x
perf/fork-exec.us_per_fork_exec             322.2        701.1       2441.9    3.48x
perf/page-fault.ns_per_fault                869.1      11238.9       8187.5    0.73x
perf/syscall.ns_per_getpid                   60.2         59.7        199.8    3.35x
```

Read the two L1 columns against each other. The L0 column is there to
show what the nesting itself costs, and it is not small: an ordinary KVM
guest's page faults go from 891ns to 8850ns once its host is itself a
guest, because nested EPT has to be walked twice.

PVM beats nested KVM on exactly the axis it claims to: page faults
(0.73x), because it shadows page tables rather than nesting EPT. Context
switches came out at 0.88x in one run and 1.27x in another, so treat that
row as noise rather than a result -- the variance under nesting is larger
than the difference. It loses on fork+exec (3.4x), which is address
spaces being created and torn down, the most expensive thing a shadow
MMU does.

### Where the time actually goes

`perf kvm stat` over a perf-suite run under PVM, with the exit reasons the
pvm_trace.h port added and a quiet guest console (see below for why that
matters):

```
VM-EXIT                Samples  Samples%   Time%    Avg time
PF excp                 356920    75.11%   27.28%     3.07us
HC_TLB_INVLPG            46964     9.88%    1.24%     1.06us
GP excp                  34625     7.29%    3.93%     4.56us
INTERRUPT                15449     3.25%    0.54%     1.41us
HC_IRQ_HALT               8199     1.73%   64.23%   314.67us
HC_WRMSR                  5262     1.11%    0.21%     1.61us
HC_LOAD_PGTBL             2707     0.57%    1.80%    26.69us
HC_IRQ_WIN                1782     0.38%    0.05%     1.16us
ERETU                     1548     0.33%    0.66%    17.11us
HC_LOAD_GS                1209     0.25%    0.03%     1.03us
```

### The syscall number, against the right baseline

3.3x was the wrong comparison. A PVM guest runs with KPTI forced off: its
kernel/user page-table separation comes from the hypervisor's shadow MMU,
not from PTI. Comparing it against an ordinary guest that also has PTI off
compares a kernel with that separation against one without.

Against a guest that has it, on the same machine, same host, same nesting:

```
                       round 1   round 2
KVM, pti=off             59.6      58.2 ns
KVM, pti=on             150.9     152.0 ns      <- KPTI costs ~92ns
PVM (pti off by design) 196.6     200.4 ns

PVM / KPTI-enabled KVM   1.30x     1.32x
```

**1.3x, not 3.3x**, and stable to two decimal places across runs. Both pay
for the same thing: an address-space switch on every syscall. KPTI writes
CR3 on entry and again on exit; the switcher writes it once, because the
guest's user and supervisor shadow page tables are separate tables.

Reading `entry_SYSCALL_64_switcher` accounts for the remaining ~46ns:
beyond the CR3 write it does a second `swapgs`, an `rdgsbase` and a
`wrgsbase`, and about fourteen stores into the PVCS to hand the guest its
entry state. That is the paravirtual protocol's bookkeeping, and it is
where an optimisation would have to come from -- not from the CR3 write,
which is the design.

### The exit histogram

**There is no SYSCALL row.** Not a single guest syscall reached the host
across two million `getpid()` calls, so the switcher's direct
user-to-supervisor switch works exactly as designed. The earlier guess --
that the direct switch was inhibited and every syscall was taking a full
exit -- was wrong. The 200ns is the switcher's own path: about 140ns more
than a bare `syscall`/`sysret`, spent saving and restoring state through
the PVCS. That is the price of the design, not a bug in it.

`HC_IRQ_HALT` dominates *Time%* and means the guest is idle, not busy.

### The #GP exits are mostly the harness

The first run of this measured 90188 #GP exits at 12.94% of time, and
breaking them down by instruction -- the `kvm_emulate_insn` tracepoint
already carries the bytes, so this needed no kernel change -- said:

```
40672  out dx        37749  in dx        12861  wrmsr
```

Port I/O, 85% of it. That is the 16550 serial console: every character
costs a poll of the line status register and a write to the transmit
register, and each is a #GP the host emulates. Booting the guest with
`quiet loglevel=0` took #GP from 90188 to 34625 and its share of time
from 12.94% to 3.93%. The perf suite now boots quiet; the harness still
needs the console for its result line, which is forty lines rather than
thirty thousand characters.

What is left is real:

```
12718  wrmsr         11145  out dx        9877  in dx
```

and the `kvm_msr` tracepoint names it: **MSR 0x6e0, `MSR_IA32_TSC_DEADLINE`,
10047 writes.** `lapic_next_deadline()` armed the timer with
`native_wrmsrq()`, which bypasses paravirt on purpose -- right on real
hardware, but on a PVM guest it is a raw `wrmsr` at CPL3, so a #GP and a
trip through the host's x86 emulator at 4.56us against 1.61us for the
hypercall.

Fixed, and the fix is free for everyone else:

```
                    before   after
wrmsr emulated       12718     541
MSR 0x6e0 writes     10047       0
GP excp exits        34625   23030   -33%
HC_WRMSR exits        5262   17307
```

The write is now conditional on `cpu_feature_enabled(X86_FEATURE_KVM_PVM_GUEST)`,
which is an alternative-patched branch, and the feature sits in the
disabled mask when `!CONFIG_PVM_GUEST`. On a kernel without PVM,
`lapic_next_deadline()` compiles to the same eleven instructions it did
before -- checked in the disassembly -- so nothing is paid for this
anywhere else. Routing it through `wrmsrq()` unconditionally would not
have been acceptable: `paravirt_write_msr()` is a `PVOP_VCALL2`, a real
indirect call rather than an alternative patched inline, which is exactly
why upstream writes it natively.

Keep it in proportion: the guest-visible numbers did not move, and were
not expected to. This benchmark does not stress timers, and run-to-run
variance under nesting is larger than the saving. What was bought is host
CPU -- about 3us per timer arm, and a third of the guest's remaining #GP
exits -- on a path whose cost rises with vCPU count and with the number of
guests on the host, neither of which this benchmark varies.

The remaining `in`/`out` are device probing and the result line; `PF excp`
and `HC_LOAD_PGTBL` are the shadow MMU doing its job, and are inherent.

These numbers are all under nesting: the PVM host itself runs in a VM, so
its shadow page-table walks are virtualised too. On bare metal the
fork+exec figure in particular should look different. Measuring that
needs a machine one is willing to reboot.

## Where fork+exec's 3.5x goes

`perf stat` over the shadow MMU tracepoints, same run under each vendor
(`make mmu`, or `scripts/run-l1.sh mmu pvm`):

```
                            PVM      nested KVM
kvm_mmu_get_page          13997             332
kvm_mmu_prepare_zap_page   7448             316
kvm_mmu_sync_page         86862               0
kvm_mmu_unsync_page       86862               0
fast_page_fault               0               2
```

Two things stand out and the second is the answer.

Shadow page allocation is 42x higher, which is inherent: PVM builds page
tables the hardware would have walked for it.

But sync/unsync is 86862 against **zero**, and it is a class of work EPT
does not have at all. When the guest writes into one of its own page
tables, the host is write-protecting that page: the write faults, the
host marks the shadow page unsync so the guest can carry on writing, and
later re-walks all 512 entries to re-validate them against the guest's.
The two counts being exactly equal says every unsync is paid for with a
resync, and 86862 syncs against 2707 `HC_LOAD_PGTBL` is about 32 resyncs
per page-table load -- consistent with each CR3 load resyncing everything
the guest dirtied since the last one.

That is precisely the work a paravirtual MMU removes. The guest already
tells the host about TLB operations by hypercall; what it does not tell
it about is page-table *updates*, so the host has to discover them by
write-protection and then reconstruct what changed. A guest that declared
its updates would let the host skip the write-protection, the fault, the
unsync and the resync.

It is also the measurement to optimise against. With each worker pinned
to its own vCPU -- `runtime.NumCPU()` alone lets the Go scheduler pile
them onto a couple of CPUs and measure the guest's scheduler instead:

```
        fork-exec(us)   page-cycle(ns)
pvm  1        1846            4709
pvm  8         874            1531
intel 1        483             742
intel 8        129             237

PVM/KVM     1 vCPU   8 vCPU
fork-exec    3.82x    6.78x
page-cycle   6.35x    6.46x
```

fork+exec is the only figure in this document that gets worse with scale,
and pinning made the trend cleaner rather than weaker.

### What a PV MMU would actually buy

Profiling the host during `perf/parallel-fork` at eight vCPUs, before
committing to a protocol change:

```
28.55%  kvm_mmu_page_fault
27.43%  asm_fred_entry_from_kvm        (self)
24.64%  queued_write_lock_slowpath     (7.04% self)
22.13%  paging64_page_fault
21.17%  x86_emulate_instruction
19.94%  pvm_handle_exit
17.60%  queued_spin_lock_slowpath      (self)
 7.05%  emulator_read_write
 5.97%  emulator_write_guest
```

`emulator_write_guest` is the guest storing into its own page tables and
the host emulating the store, because the page is write-protected. That
is the work a PV MMU deletes outright, and with it most of the 28% in
`kvm_mmu_page_fault`, since those faults are what the write-protection
generates.

But the two lock slowpaths together are the larger share, and they are
`kvm->mmu_lock` contended across vCPUs. A PV MMU helps them only
indirectly, by taking the lock fewer times. What remains after that is
architectural: KVM's answer to mmu_lock contention was the TDP MMU, with
per-SPTE cmpxchg under RCU, and shadow paging cannot use it.

So the case for the protocol work is good but not unlimited: it removes a
whole class of work, and the residual is a lock that shadow paging in KVM
has never had a good answer for.

One caveat on reading the table: `asm_fred_entry_from_kvm` showing 27%
*self* in a small assembly stub is more likely the PMU's NMI landing
while the CPU is in the guest and being attributed to the host's
re-entry path than it is real time spent there. Host sampling cannot see
inside guest execution, which is the same limitation that made profiling
the switcher useless earlier.

## The ceiling: a PVM host cannot use KPTI

Measured, not inferred.  Booting L1 with `pti=on`:

```
kvm_pvm: Support for host KPTI is not included yet.
insmod: ERROR: could not insert module: Operation not supported
```

It is an explicit refusal in `pvm_init()`, and the switcher's CR3 macros
are `ALTERNATIVE`d out under `X86_FEATURE_PTI` to match.  The reason is in
`calling.h`: the switcher would have to reach the host CR3 in the IST path
before GSBASE is fixed up, and the obvious way to do that reads the TSS
through the CPU entry area -- which an SEV guest breaks by rewriting
TSS.IST at run time.

Everything in this document works because this machine's CPU reports
`meltdown: Not affected`, so PTI is off and the switcher is live.  On a
Meltdown-affected host -- Intel before roughly 2019 -- PTI is on by
default and **PVM cannot run at all**.  For the "better than what cloud
providers have now" question this is the first gate, ahead of any
performance number.

## What hiding PKU costs

`pvm_set_cpu_caps()` used to advertise PKU with a comment calling it "a
temporary fix": exposing it keeps the guest's XCR0 equal to the host's,
which avoids an XCR0 switch on every entry and exit. It is no longer
advertised, because there is one hardware PKRU and PVM needs it to be
three things at once — the host's, the guest's architectural one, and the
zero it forces for the duration of the guest — so there is no guest PKRU
to advertise.

**The cost is not currently measurable here.** An earlier version of this
section reported it as +28% on page faults, +31% on parallel fork and
+58% on the parallel page cycle, from medians of five runs on each side.
Those numbers do not survive scrutiny and are withdrawn.

What went wrong is worth keeping, because it applies to every performance
claim this testbed makes. Each run boots a fresh L1, and there are two
separate sources of variation:

- *Within* a sweep of five runs, most metrics vary by 4-6%. But
  `perf/context-switch` varies by **116%** — it is not a usable
  measurement as it stands, and a single outlier in it once produced an
  apparent +91% effect that vanished on the next sweep.
- *Between* sweeps of **the same build**, an hour apart, `page-fault`
  read 14810, then 11280, then 10020 ns — a spread of nearly 50%,
  swamping every delta above.

The second one is the killer, and the original sweep ran all five runs of
one build and then all five of the other, so that drift landed entirely
on one side. `perf-matrix.sh` now reports the within-sweep spread next to
every median, interleaves vendors so both see the same drift, and refuses
to call a delta a regression unless it also lands outside the range the
baseline's own runs covered.

What is still true, and rests on the code rather than on these numbers:
exposing PKU keeps the guest's XCR0 equal to the host's and so avoids an
XCR0 switch on every entry and exit, and in this testbed each such switch
is a VM exit to L0 because the PVM host is itself a guest. So there is a
real cost and this configuration exaggerates it. How large it is, here or
on bare metal, is unmeasured.

The conclusion does not depend on the number: correctness is not
negotiable, the feature stays hidden, and the way to get the performance
back is to finish separating the three PKRU users rather than to
advertise state that is not kept.

## What the measurements can and cannot resolve

Three things were wrong at once, and finding them cost a withdrawn set of
numbers. All three are fixed; what is left is a per-metric resolution
floor that has to be respected.

**The benchmark.** `perf/context-switch` ping-ponged between two
goroutines over unbuffered channels, which measures the Go scheduler, not
the kernel: a roundtrip was sometimes a userspace goroutine switch and
sometimes a futex sleep and wake. It varied by **116%** across five runs
of the same build, and one outlier invented a 91% regression that did not
exist. It is now two processes pinned to the same CPU passing a byte
through a pair of pipes — two real context switches per roundtrip. Note
the metric changed name to `ns_per_pipe_roundtrip`, and the number is an
order of magnitude larger than the old one because the old one was
usually not measuring the kernel at all.

**The core lottery.** This is a hybrid CPU: CPUs 0-11 are P-cores at
5.2-5.4GHz, 12-19 are E-cores at 4.1GHz. Where the scheduler put qemu's
threads changed the answer by tens of percent from run to run. Both
runners now pin to the fastest core type, detected by MAXMHZ rather than
hardcoded (`PIN_CPUS` overrides, `PIN_CPUS=none` disables).

**Transparent huge pages.** `perf/page-fault` was bimodal: a THP faults
in 512 base pages at once, so whether the mapping got one changed the
result by tens of percent. It now asks for `MADV_NOHUGEPAGE` and measures
base-page faults.

Measured after all three, two sweeps of the same build back to back,
medians of five runs each:

```
metric                                     sweep 1   sweep 2   between   within
perf/context-switch.ns_per_pipe_roundtrip  23200.0   22720.0     -2.1%    6-8%
perf/fork-exec.us_per_fork_exec             2652.0    2701.0     +1.8%   8-11%
perf/syscall.ns_per_getpid                   197.9     203.3     +2.7%      8%
perf/page-fault.ns_per_fault               11800.0   10040.0    -15.0%   8-11%
```

So: **treat anything under about 10% as noise, and under 15% for
page-fault.** page-fault is still the least trustworthy metric here and
something beyond THP is moving it. `perf-matrix.sh` prints the
within-sweep spread next to every median and will not call a delta a
regression unless it also lands outside the range the baseline's own runs
covered, so the tool now refuses to make the claim rather than making it
wrongly — but a human reading two sweeps still has to apply the floor
above.

## objtool: what is left, and what each one is

A PIE build with `PVM_GUEST=n` and `KVM_PVM=n` produces exactly **one**
warning. So of the five this port used to carry, one belongs to the PIE
series and four belonged to the PVM guest. An earlier version of this
document had that backwards.

Two of the four are fixed: `pvm_early_event` and `irqentry_enter` reported
"RET before UNTRAIN", and they were right — the guest's event entry paths
called into C without `IBRS_ENTER`/`UNTRAIN_RET`/`CLEAR_BRANCH_HISTORY`,
which every native entry runs first. Adding them removed both warnings,
because the untraining is now actually there.

Three remain:

**`___bpf_prog_run+0x2: ignoring unreachables due to jump table quirk`** —
PIE's, not PVM's. Present with `PVM_GUEST=n`. Belongs to whoever upstreams
the PIE series.

**`entry_SYSCALL_64+0x39: stack state mismatch: cfa1=-1+0 cfa2=4-128`** —
the PVM guest's, and localised but not fixed. `entry_SYSCALL_64_pvm` jumps
into the middle of `entry_SYSCALL_64` at
`entry_SYSCALL_64_after_hwframe`, twice: once directly when there is no
async exception, and once after handling one. Cutting both jumps and
pointing them at a local `ud2` moves the warning to
`entry_SYSCALL_64_pvm` itself with `cfa1=4+0 cfa2=4-128`, which proves it
is the PVM path and that the two predecessors disagree with each other,
not merely with the native path. `PUSH_IRET_FRAME_FROM_PVCS` switches
stacks, which loses objtool's CFA. An `UNWIND_HINT_IRET_REGS` after the
frame is complete was tried and did **not** resolve it — it changed `cfa2`
and left `cfa1` undefined — so it was reverted rather than left in place
as an annotation that does not describe the flow.

**`pvm_event+0x6a: call to {dynamic}() leaves .noinstr.text section`** —
the PVM guest's. The indirect dispatch out of `pvm_event`. Every current
target was verified to be `noinstr`, so this is regression-prevention
rather than a live gap.

## Known gaps in the port

- `arch/x86/boot/compressed/` has not been ported, so the bzImage path
  has not been made to work for a PVM guest. Stage 0's bzImage boot is
  still meaningful: it is the non-PVM path, and it must keep working.
- `segment.h`'s `vdso_read_cpunode` RDTSCP alternative is unported. The
  `syscall/getcpu-affinity` case is the one that will notice.
- Seven objtool warnings remain on the PIE build, two of them in PVM
  code. The other five have not been checked against a clean v7.3-rc2
  build and may well predate the port; nobody should spend time on them
  before that comparison is made.

  **`pvm_event+0x6a: call to {dynamic}() leaves .noinstr.text section`**

  This is the `func(regs, vector)` in `pvm_handle_sysvec()`, inlined into
  `pvm_event()`. The targets are all `DEFINE_IDTENTRY_SYSVEC` handlers,
  which *are* noinstr and do their own `irqentry_enter()`, so the call is
  correct — objtool simply cannot see through a function pointer.

  It has exactly one mechanism for this, and it is hardcoded to one
  table: `noinstr_call_dest()` falls through to `pv_call_dest()`, which
  walks `file->pv_ops[]` and checks every recorded target is in a noinstr
  section. `pvm_sysvec_table` is structurally the same thing and gets
  none of that.

  So there are two real fixes and one non-fix. Generalise objtool's
  pv_ops machinery to any table declared noinstr-only — the right answer,
  and an upstream-sized change. Or replace the table with a `switch`, so
  every target is a direct call objtool can follow, which is what the
  syscall dispatch did for a related reason; but `pvm_install_sysvec()`
  is a runtime registration API, so that is not a mechanical change. The
  non-fix is wrapping the call in `instrumentation_begin()`: it would
  silence the warning by asserting something false, and the handlers open
  their own instrumentation region anyway.

  Until then it is a live gap, not cosmetic: it means kprobes and ftrace
  can fire in a context where the entry code has not finished setting up.

  **`pvm_early_setup+0x1e2: relocation to !ENDBR: this_cpu_cmpxchg16b_emu`**

  From `pvm_early_patch()`, which takes the address of the emulation
  helper in order to overwrite it. Whether this needs an ENDBR or an
  `ANNOTATE_NOENDBR` depends on whether anything ever calls that address
  indirectly, which has not been checked.

## Order to attack it in

1. ~~`make stage0`~~ -- done, green on both entry paths.
2. `make stage1`. Needs `mmdebstrap` for the L1 root filesystem. The host
   side is the less-changed half, so this should be easier than it
   sounds.
3. ~~`make stage1`~~ -- done.
4. ~~`make stage2`~~ -- done, 31/31.
5. `make full`, then `make perf` for a PVM-versus-KVM comparison on the
   same machine. See docs/DEBUGGING.md for how to see into an early
   crash, which is where every bug so far has been.
4. `make full`, then `make perf` for a PVM-versus-KVM comparison on the
   same machine.
