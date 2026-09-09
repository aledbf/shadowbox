#!/bin/bash
#
# Runs inside L1, as the PVM host.  Loads kvm-pvm, states plainly whether
# it came up, then boots the guest under it and forwards the guest's
# serial output to L1's console -- which is L0's log file.
#
# Every line it prints is prefixed so the two kernels' output can be told
# apart in a single log.

set -u

# Straight to the console.  Routed through journald, the console output is
# rate limited, and what gets dropped is the end -- which is where the
# diagnostics are.
exec >/dev/console 2>&1

say() { echo "L1: $*"; }

say "kernel: $(uname -r)"
say "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2-)"

# Which vendor to run under.  Both are modules and both travel on the
# payload share, because the rootfs is bootstrapped once and the kernel
# version string -- and so the path modprobe would search -- moves with
# every commit.  insmod by path sidesteps that entirely.
VENDOR="$(sed -n 's/.*pvmtest\.vendor=\([^ ]*\).*/\1/p' /proc/cmdline)"
VENDOR="${VENDOR:-pvm}"
case "$VENDOR" in
pvm)   mod=/mnt/payload/kvm-pvm.ko ;;
intel) mod=/mnt/payload/kvm-intel.ko ;;
*)     say "unknown vendor: $VENDOR"; poweroff -f ;;
esac

say "loading $VENDOR from $mod"
if ! insmod "$mod" 2>&1 | sed 's/^/L1: insmod: /'; then
	say "insmod failed"
fi
lsmod | grep -E '^kvm' | sed 's/^/L1: lsmod: /'

dmesg | grep -i -E 'pvm|kvm' | tail -40 | sed 's/^/L1: dmesg: /'

if [ ! -e /dev/kvm ]; then
	say "no /dev/kvm -- the PVM host did not register"
	echo "PVMTEST-RESULT: fail stage=1 reason=no-kvm-device"
	poweroff -f
fi
say "/dev/kvm present"

# The payload share is already mounted: the stub in the image did it
# before exec'ing this script, which is how this script got here at all.
# Mounting it a second time fails with "no channels available for device
# payload", and this script used to treat that as fatal.

SUITE="$(sed -n 's/.*pvmtest\.suite=\([^ ]*\).*/\1/p' /proc/cmdline)"
SUITE="${SUITE:-default}"

say "qemu: $(qemu-system-x86_64 -version 2>&1 | head -1)"
ls -l /mnt/payload | sed 's/^/L1: payload: /'

# The guest dies before its first printk, so the only account of what
# happened is the host's.  KVM's tracepoints are the closest thing to a
# debugger here.
T=/sys/kernel/tracing
[ -d "$T" ] || T=/sys/kernel/debug/tracing
if [ -d "$T" ]; then
	echo 0 > "$T/tracing_on" 2>/dev/null
	echo > "$T/trace" 2>/dev/null
	echo 32768 > "$T/buffer_size_kb" 2>/dev/null
	echo 1 > "$T/events/kvm/enable" 2>/dev/null && say "kvm tracepoints enabled"
	echo 1 > "$T/tracing_on" 2>/dev/null
fi

case "$SUITE" in
full|perf|all) GUEST_TIMEOUT=1800 ;;
*)          GUEST_TIMEOUT=120 ;;
esac
say "guest timeout: ${GUEST_TIMEOUT}s"

APPEND="console=ttyS0,115200 earlyprintk=serial,ttyS0,115200 panic=-1 oops=panic pvmtest.suite=$SUITE pvmtest.tag=$VENDOR-guest"
# Only a PVM run must have relocated itself; under kvm-intel the same
# image is an ordinary guest and belongs at the usual address.
[ "$VENDOR" = pvm ] && APPEND="$APPEND pvmtest.expect=pvm"

# Two machine types, because they differ in exactly the way that matters.
#
# q35 runs SeaBIOS first, in real mode, and SeaBIOS enables interrupts --
# at which point the host has to inject an IRQ into a guest that is not in
# PVM mode yet.  do_pvm_event() does not support that: it warns and raises
# a triple fault, which is the KVM_EXIT_SHUTDOWN we see.
#
# microvm has no firmware.  The kernel is entered directly, with
# interrupts off, and stays that way until it is in long mode -- so
# nothing is ever injected in non-PVM mode.  If this one gets further,
# that confirms where the problem is.
run_guest() { # $1=tag, rest=machine args
	local tag="$1"; shift
	rm -f /tmp/guest.log /tmp/qemu.log
	say "=== booting the PVM guest on $tag ==="
	# Bounded, so that a guest that wedges its vCPU thread does not take
	# the whole agent down with it and cost us the host side diagnostics.
	# The bound has to fit the suite: the stress and perf cases allow
	# themselves several minutes each, and 45s was chosen back when the
	# guest was dying in under a second.
	timeout -k 5 "$GUEST_TIMEOUT" qemu-system-x86_64 "$@" \
		-cpu host -smp 2 -m 1G \
		-kernel /mnt/payload/guest-vmlinux \
		-initrd /mnt/payload/initrd.cpio.gz \
		-append "$APPEND" \
		-display none -monitor none -serial file:/tmp/guest.log \
		-no-reboot < /dev/null > /tmp/qemu.log 2>&1
	local rc=$?
	say "$tag: qemu exited $rc"
	[ -s /tmp/qemu.log ] && sed "s/^/L1: $tag qemu: /" /tmp/qemu.log
	if [ -s /tmp/guest.log ]; then
		sed "s/^/G[$tag]: /" /tmp/guest.log
	else
		say "$tag: the guest produced no serial output at all"
	fi
}

# Core dumps off, on purpose.  vfs_coredump() is what sleeps, and it is
# sleeping with a leaked preempt count -- so the dump never finishes, qemu
# never exits, and the agent waits on it forever.  Without a dump the
# SIGSEGV just kills qemu and we get to keep debugging.
#
# print-fatal-signals gives the faulting address, RIP and error code for
# the killed process, which is the one line that says whether this is qemu
# faulting or a PVM guest fault reaching the host's own #PF handler.
ulimit -c 0
echo core > /proc/sys/kernel/core_pattern 2>/dev/null
echo 1 > /proc/sys/kernel/print-fatal-signals 2>/dev/null

run_guest q35 -machine q35,accel=kvm

if [ -d "$T" ]; then
	echo 0 > "$T/tracing_on" 2>/dev/null
fi

# do_pvm_event() warns once per rate-limit window when the VMM injects an
# event while the vCPU is still in the non-PVM bootstrap mode.  Its
# presence or absence says whether that path was reached at all.
say "--- what the host said about the guest ---"
dmesg | grep -iE "PVM:|non-PVM mode" | tail -6 | sed 's/^/L1: pvm: /' ||
	say "(nothing)"
say "--- injection warnings ---"
if ! dmesg | grep -i "non-PVM mode" | tail -3 | grep . | sed 's/^/L1: warn: /'; then
	say "no 'non-PVM mode' warning"
fi

# The backtrace is the thing.  Print the region around it and nothing
# else: a full dmesg dump is long enough that the tail of it is what gets
# lost, and the tail is the part that matters.
# The most informative line of all, if qemu died: the kernel logs the
# faulting address, instruction pointer and error code for an unhandled
# user signal.  A PVM guest runs at hardware CPL3 inside the qemu thread,
# so a guest fault that reaches the host's own #PF handler looks exactly
# like qemu faulting -- and this line is what tells the two apart.
say "--- did qemu fault, and where ---"
if ! dmesg | grep -E "segfault|trap [a-z]+ ip|traps:" | tail -5 | grep . |
		sed 's/^/L1: sig: /'; then
	say "no segfault reported"
fi

say "--- kernel complaints from the guest run ---"
if ! dmesg | sed -n '/scheduling while atomic\|BUG:\|general protection fault\|unable to handle/,+12p' \
		| head -30 | grep . | sed 's/^/L1: bug: /'; then
	say "no BUG, GPF or fault in dmesg"
fi

# The teardown thread and the mmu-notifier unmap storm are the loudest
# things in the buffer and say nothing.  Everything else, in order, is
# what actually happened -- and picking a thread by name gets it wrong,
# because the qemu IO thread and the vCPU threads share it.
if [ -d "$T" ]; then
	say "--- last 70 trace lines, teardown and unmaps removed ---"
	grep -vE "kvm-nx-lpage|kvm_unmap_hva_range|kvm_hv_stimer_cleanup|kvm-pit|kvm_(pic|ioapic)_set_irq|kvm_set_irq" "$T/trace" |
		tail -70 | sed 's/^/L1: kvm: /'
fi

say "done"
poweroff -f
