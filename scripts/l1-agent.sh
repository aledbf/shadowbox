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

# The guest image and initrd are handed in on a 9p mount so the L1 root
# filesystem does not have to be rebuilt every time the guest kernel is.
mkdir -p /mnt/payload
if ! mount -t 9p -o trans=virtio,version=9p2000.L payload /mnt/payload; then
	say "could not mount the payload share"
	echo "PVMTEST-RESULT: fail stage=1 reason=no-payload"
	poweroff -f
fi

SUITE="$(sed -n 's/.*pvmtest\.suite=\([^ ]*\).*/\1/p' /proc/cmdline)"
SUITE="${SUITE:-default}"

say "booting the PVM guest (suite=$SUITE)"
qemu-system-x86_64 \
	-machine q35,accel=kvm \
	-cpu host \
	-smp 2 -m 1G \
	-kernel /mnt/payload/guest-vmlinux \
	-initrd /mnt/payload/initrd.cpio.gz \
	-append "console=ttyS0,115200 earlyprintk=serial,ttyS0,115200 panic=-1 oops=panic pvmtest.suite=$SUITE pvmtest.tag=pvm-guest pvmtest.expect=pvm" \
	-nographic -no-reboot -display none -serial mon:stdio \
	< /dev/null 2>&1 | sed 's/^/G: /'

say "guest exited"
poweroff -f
