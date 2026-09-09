# Status

Written alongside the 7.3 port, before any of it had been booted.

## What has been run

Nothing yet. The guest kernel builds clean with and without
`CONFIG_X86_PIE`, and `kvm-pvm` builds as a module, but no image produced
by this tree has been executed. Every claim below is about what the code
is supposed to do.

## Known gaps in the port

- `arch/x86/boot/compressed/` has not been ported, so the bzImage path
  has not been made to work for a PVM guest. Stage 0's bzImage boot is
  still meaningful: it is the non-PVM path, and it must keep working.
- `segment.h`'s `vdso_read_cpunode` RDTSCP alternative is unported. The
  `syscall/getcpu-affinity` case is the one that will notice.
- Seven objtool warnings remain on the PIE build, two of them in PVM
  code:
  - `pvm_event+0x6a: call to {dynamic}() leaves .noinstr.text section`
  - `pvm_early_setup+0x1e2: relocation to !ENDBR: this_cpu_cmpxchg16b_emu`
  The other five predate the PVM changes and want checking against a
  clean v7.3-rc2 build before anyone spends time on them.

## Order to attack it in

1. `make stage0` with the smoke suite. This is the first time any of the
   PIE work runs. Expect it to fail; the serial log up to the failure is
   the useful output.
2. `make stage0` with the default suite, until it is green. At this point
   the port is a working ordinary kernel, which is a prerequisite for it
   being anything else.
3. `make stage1`. The host side is the less-changed half, so this should
   be easier than it sounds.
4. `make stage2`. This is the first execution of the switcher.
