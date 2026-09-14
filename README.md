# PVM testbed

Builds the two kernels the PVM port needs and runs a suite against them.
Nothing here touches this machine's running kernel.

**Picking this up cold?** `docs/BRIEFING.md` is where it stands: the
measurements, what is structural and what is not, what has already been tried
and why it failed, and the ways to measure that do not fool you. Read it before
proposing a performance change or a smaller diff. `docs/STATUS.md` is the
bring-up record and `docs/DEBUGGING.md` is how to see into a guest that dies
before it can print.

Only one step can need root, and only on some machines: building the L1
root filesystem. `mmdebstrap` does it unprivileged where unprivileged
user namespaces are allowed, but Ubuntu 24.04 and later restrict those by
default. `scripts/build-rootfs.sh` picks between `unshare`, `fakechroot`
and `root`, says which it picked, and chowns its output back to the
invoking user when it ran under sudo. Everything else — both kernel
builds, the initrd, and every boot — runs as you.

## The shape of it

PVM's whole claim is that a host needs no VMX and no EPT — it runs the
guest at hardware CPL3 and shadows its page tables. That is what makes
this testable on a laptop without rebooting it:

```
L0   this machine, its own kernel, ordinary KVM
 └── L1   the PVM *host*: our host kernel, booted as a normal KVM guest.
     │    Needs no nested VMX, because kvm-pvm uses none.
     └── the guest under test, booted by qemu inside L1 through kvm-pvm
```

Stage 0 skips all of that. The guest kernel is built PIE and PVM-capable
but must still boot as an ordinary KVM guest — `pvm_detect()` returns
false and the relocation is a no-op — so stage 0 boots it directly on L0
and exercises every line of the PIE and early-boot work without a PVM
host existing yet. It is the cheapest useful signal in the whole tree and
it is where to start.

## Stages

| stage | what it proves | needs |
| --- | --- | --- |
| `make stage0` | the guest kernel boots as a plain KVM guest, both through the PVH ELF note and as a bzImage | qemu |
| `make stage1` | the host kernel boots in L1 and `kvm-pvm` registers a `/dev/kvm` | qemu, mmdebstrap |
| `make stage2` | the guest boots **under PVM** and passes the default suite | as above |
| `make full` | stage 2 plus the long churn and stress cases | as above |
| `make perf` | the same measurements on PVM and on plain KVM, side by side | as above |

Start with `make deps`; it names anything missing and the package that
carries it.

## Batteries

Everything past a single stage runs as a battery: a file under
`batteries/` that says which boots to do and what counts as passing, run by
`tools/pvmtest` (`make battery B=<name>`).

```
run      default   suite=default
run      la57      suite=default l1=tcg-la57 allow-fail=time/monotonic
run      no-kpti   suite=failclosed pti=on expect=refuse-load reason="without KPTI"
matrix   perf      suite=perf vendor=pvm,intel cpus=1,2,4,8,16 reps=5 host=timing
ab       dpf       suite=perf cpus=1,8 reps=5 a.guest=pvm_direct_pf=off b.guest=pvm_direct_pf=on
```

The runner refuses an item that needs another host build (`host=stats` or
`timing`), keeps every log and a manifest (kernel and testbed revisions,
the battery itself) in `out/results/<battery>-<time>/`, writes
`summary.tsv` with a verdict per boot -- known failures listed, kernel log
checked by `scripts/sanitize-log.sh`, selftests by `check-selftests.sh` --
and reduces `matrix` and `ab` items to medians, deltas and whether the two
sides' ranges overlap.  `pvmtest list <battery>` prints the boots and their
exact `run-l1.sh` command lines without running anything; `pvmtest stats
<log>` gives per-case counter deltas of a `stats=on` boot.

## Layout

```
configs/        kernel config fragments: common, guest, host
scripts/        build and run; each does one thing and says what it did
batteries/      what to run and what passing means, for tools/pvmtest
tools/pvmtest/  the battery runner (Go, standard library only)
initrd/         the guest's entire user space, in Go
out/            everything built (gitignored)
out/logs/       one serial log per boot; this is the primary evidence
```

## The initrd

The guest's user space is a single static Go binary running as PID 1.
There is no shell and no libc in the image. That is deliberate: a guest
that prints `PVMINIT: up` has really executed an ELF load, real page
faults, and a real syscall return through whatever entry path the kernel
was built with, and none of it was smoothed over by a rescue shell.

It mounts `/proc`, `/sys`, `/dev` and `/tmp`, prints where the kernel
actually landed, runs the selected suite, prints one machine-readable
result line, and powers the machine off. Every path through it ends in a
result line and a power off — a VM that hangs after a failure is a three
minute wait for an answer it already had.

The scripts outside key off exactly one line:

```
PVMTEST-RESULT: ok tag=stage0-pvh-default suite=default pass=19 fail=0
```

## Suites

`smoke` is "did it boot": alive, procfs, SMP bringup, no oops, exec, the
kernel map address, an anonymous mmap, a syscall round trip, monotonic
time. `default` adds the everyday correctness cases. `full` adds the
long-running churn. `perf` measures and never asserts.

Suites nest: `default` runs everything in `smoke`, `full` everything in
`default`. `perf` stands alone.

## Reading the result

The most informative single line in a run is this one:

```
PVMINIT: _text at 0xffffffff81000000
```

An ordinary kernel is mapped in the top 2GB. A PVM guest may not use that
range at all, so under PVM this address must be somewhere else — it is
the one number that says whether the early relocation ran. Stage 0
expects the usual address; stage 2 passes `pvmtest.expect=pvm` and
`pvm/relocated` turns it into a hard failure.

## The host side

`make hosttests` runs two things inside L1, against the loaded vendor
module, with no guest of ours involved:

- `hosttests/pvm_abi_test.c`, which drives `/dev/kvm` directly. The PVM
  MSRs, the PVCS pinning and the memslot lifetime around it are reachable
  only from the VMM; the guest-side suite runs at guest CPL3 and cannot
  touch any of it.
- a curated subset of the kernel's own KVM selftests, listed in
  `configs/kvm-selftests.txt`, with the outcome of each recorded per
  vendor in `configs/kvm-selftests-expect.txt`. Any difference from the
  recorded outcome fails the run — including a test that starts *passing*,
  which means a gap closed and the file now says the opposite of the
  truth.

Both vendors are run, because a selftest that behaves identically under
`kvm-intel` is telling us about the testbed rather than about PVM.

## What is not covered

There are still no kvm-unit-tests.

`make security` checks that a guest user process cannot reach guest kernel
memory or the host's window, but the security argument as a whole wants
review rather than a test run.

The performance numbers are guest-visible only. Where the cost actually
lands — host CPU spent in the shadow MMU — needs `perf` on L1, not a
number the guest can print.
