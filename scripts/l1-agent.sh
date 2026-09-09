#!/bin/bash
#
# Runs inside L1, as the PVM host.  Loads kvm-pvm, states plainly whether
# it came up, then boots the guest under it and forwards the guest's
# serial output to L1's console -- which is L0's log file.
#
# Every line it prints is prefixed so the two kernels' output can be told
# apart in a single log.

set -u
say() { echo "L1: $*"; }

say "kernel: $(uname -r)"
say "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2-)"

# CONFIG_KVM_PVM is built in, not a module, in the host configuration the
# testbed builds -- so modprobe failing here means nothing on its own and
# /dev/kvm below is the real answer.  Piping through sed would have hidden
# modprobe's status behind sed's, so capture it first.
out="$(modprobe kvm-pvm 2>&1)"; rc=$?
[ -n "$out" ] && echo "$out" | sed 's/^/L1: modprobe: /'
say "modprobe kvm-pvm: exit $rc"

if lsmod | grep -q '^kvm_pvm'; then
	say "kvm-pvm loaded as a module"
else
	say "kvm-pvm is not a module (built in, in this configuration)"
fi

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

APPEND="console=ttyS0,115200 earlyprintk=serial,ttyS0,115200 panic=-1 oops=panic pvmtest.suite=$SUITE pvmtest.tag=pvm-guest pvmtest.expect=pvm"

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
	timeout -k 5 45 qemu-system-x86_64 "$@" \
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

run_guest q35 -machine q35,accel=kvm

# The decisive line, printed immediately and on its own: do_pvm_event()
# warns exactly once per rate-limit window when the VMM injects an event
# while the vCPU is still in the non-PVM bootstrap mode.
say "--- did the host complain about injection? ---"
dmesg | grep -i "non-PVM mode" | tail -3 | sed 's/^/L1: warn: /' ||
	say "no 'non-PVM mode' warning in dmesg"

say "--- host dmesg after the guest run ---"
dmesg | tail -45 | sed 's/^/L1: post: /' 
qrc=$?

# Where the host says why.  A guest that dies before its first printk
# leaves nothing of its own behind, so the hypervisor's view is all there
# is, and qemu's cpu_reset trace says what state it died in.
if [ -d "$T" ]; then
	echo 0 > "$T/tracing_on" 2>/dev/null
	say "--- last kvm tracepoints ---"
	tail -50 "$T/trace" | sed 's/^/L1: kvm: /'
fi

say "--- host dmesg since the guest started ---"
dmesg | tail -40 | sed 's/^/L1: post: /'


say "guest exited"
poweroff -f
