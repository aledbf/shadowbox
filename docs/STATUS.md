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

Stage 1 passes: the host kernel boots in L1, `kvm-pvm` registers, and
`/dev/kvm` appears. `kvm_x86_vendor_init()` WARNs 15 times on the way for
kvm_x86_ops the PVM backend does not implement -- see below.

Stage 2 reaches the switcher and dies there. The guest now gets through
SeaBIOS, through the PVH entry, sets EFER.LME and enters long mode:

```
kvm_msr: msr_write c0000080 = 0x100        EFER.LME
kvm_entry: vcpu 0, rip 0x36e9ca7           pvh_start_xen+0xd7
```

`pvh_start_xen+0xd7` is the first instruction after the `lret` into
64-bit mode, so `try_to_convert_to_pvm_mode()` has taken the vCPU out of
the bootstrap mode and that `kvm_entry` is the first time the switcher
has ever run. What happens next is that **qemu segfaults**:

```
asm_exc_page_fault -> irqentry_exit -> arch_do_signal_or_restart
  -> get_signal -> vfs_coredump -> schedule
BUG: scheduling while atomic: qemu-system-x86/229/0x00000002
```

The BUG is a symptom, not the fault: it is the coredump of a SIGSEGV'd
qemu trying to sleep. The two things it says are that the switcher
corrupted its own host process, and that it returned with a preempt count
of 2 -- two `preempt_disable()` calls that were never paired, so the path
back to the host did not unwind. That is where to look next, and it is
host-backend work rather than anything to do with the port.

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
4. `make stage2`. Reached; see above. The switcher runs once and takes
   qemu with it. The leaked preempt count is the thread to pull.
4. `make full`, then `make perf` for a PVM-versus-KVM comparison on the
   same machine.
