# PVM testbed

Builds the two kernels the PVM port needs and runs a suite against them.
Nothing here needs root, and nothing here touches this machine's running
kernel.

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

## Layout

```
configs/        kernel config fragments: common, guest, host
scripts/        build and run; each does one thing and says what it did
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

## What is not covered

Nothing here checks the host side from the host's own point of view:
there are no kvm-unit-tests, no KVM selftests, and no check that a PVM
guest cannot reach host memory. Those want a different harness, and the
security argument in particular wants review rather than a test run.

The performance numbers are guest-visible only. Where the cost actually
lands — host CPU spent in the shadow MMU — needs `perf` on L1, not a
number the guest can print.
