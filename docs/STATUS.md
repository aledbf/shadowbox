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
perf/context-switch.ns_per_roundtrip        317.9        494.1        433.2    0.88x
perf/fork-exec.us_per_fork_exec             302.9        645.7       2221.7    3.44x
perf/page-fault.ns_per_fault                891.5       8849.7       6648.3    0.75x
perf/syscall.ns_per_getpid                   59.1         62.3        206.0    3.31x
```

Read the two L1 columns against each other. The L0 column is there to
show what the nesting itself costs, and it is not small: an ordinary KVM
guest's page faults go from 891ns to 8850ns once its host is itself a
guest, because nested EPT has to be walked twice.

PVM beats nested KVM on exactly the axis it claims to: page faults
(0.75x) and context switches (0.88x), because it shadows page tables
rather than nesting EPT. It loses on fork+exec (3.4x), which is address
spaces being created and torn down, the most expensive thing a shadow
MMU does.

### Where the time actually goes

`perf kvm stat` over a full perf-suite run under PVM, with the exit
reasons the pvm_trace.h port added:

```
VM-EXIT                Samples  Samples%   Time%    Avg time
PF excp                 351735    67.35%   24.66%     2.87us
GP excp                  90188    17.27%   12.94%     5.87us
HC_TLB_INVLPG            45603     8.73%    1.25%     1.12us
INTERRUPT                12317     2.36%    0.49%     1.62us
HC_IRQ_HALT               8014     1.53%   58.39%   298.13us
HC_WRMSR                  5918     1.13%    0.27%     1.89us
HC_LOAD_PGTBL             2906     0.56%    1.44%    20.33us
HC_IRQ_WIN                2274     0.44%    0.07%     1.31us
ERETU                     1585     0.30%    0.35%     8.92us
HC_LOAD_GS                1208     0.23%    0.03%     1.15us
HC_RDMSR                   268     0.05%    0.01%     1.77us
HC_TLB_FLUSH_CURRENT        211     0.04%    0.10%    19.95us
HC_TLB_FLUSH                33     0.01%    0.00%     2.07us
```

**There is no SYSCALL row.** Not a single guest syscall reached the host
across 2 million `getpid()` calls, so the switcher's direct
user-to-supervisor switch works exactly as designed. The guess above --
that the direct switch was inhibited and every syscall was taking a full
exit -- was wrong. The 206ns is the switcher's own path: about 144ns more
than a bare `syscall`/`sysret`, spent saving and restoring state through
the PVCS. That is the price of the design, not a bug in it.

HC_IRQ_HALT dominates *Time%* and should be read as the guest being idle,
not as cost: the host is waiting for an interrupt to arrive.

Discounting it, the real distribution is:

- **PF excp**, 67% of exits and a quarter of the time. The shadow MMU,
  and each one is cheap at 2.87us. This is the tax PVM chooses to pay
  in exchange for not needing EPT, and the perf table shows it winning
  that trade against nested KVM.
- **GP excp**, 90188 of them at 5.87us each -- around 13% of all the time
  spent. Every privileged instruction the guest executes that has no
  paravirt hook traps as #GP and goes through the host's x86 emulator.
  Some of these are already hypercalls (HC_RDMSR, HC_WRMSR are right
  there in the table), so the ones left are worth identifying by
  instruction. **This is the first optimisation target**, and it is the
  cheapest kind: each one converted to a hypercall or a pv_op is 5.87us
  saved, with no design change.
- **HC_LOAD_PGTBL** at 20.33us is the expensive hypercall, and it is what
  makes fork+exec 3.4x. Address space creation is the shadow MMU's worst
  case.

Exactly one exit came back as unclassified `HYPERCALL`, so the mapping
covers essentially everything the guest asks for.

These numbers are all under nesting: the PVM host itself runs in a VM, so
its shadow page-table walks are virtualised too. On bare metal the
fork+exec figure in particular should look different. Measuring that
needs a machine one is willing to reboot.

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
