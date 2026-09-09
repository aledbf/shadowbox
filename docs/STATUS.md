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

The syscall number is the one to look at next. PVM's premise is that a
guest syscall is a direct user-to-supervisor switch that never leaves the
guest, so 206ns against 62ns is three times more than that premise
allows. Either the direct switch is being inhibited and every syscall is
taking a full exit to the PVM host, or the switcher's path is costing far
more than it should. Nothing here has been profiled yet.

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
