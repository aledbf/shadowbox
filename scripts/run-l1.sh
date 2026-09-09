#!/usr/bin/env bash
#
# Boots L1 -- the PVM host -- as an ordinary KVM guest of this machine,
# with the guest kernel handed in over 9p.  L1 needs no nested VMX: a PVM
# host uses none, which is the property this whole harness exists to
# check.

source "$(dirname "$0")/lib.sh"
check_run_deps

suite="${1:-default}"
[ -f "$OUT/images/host-bzImage" ]   || die "no host kernel -- run 'make host-kernel'"
[ -f "$OUT/images/l1-rootfs.ext4" ] || die "no L1 rootfs -- run 'make rootfs'"

# What L1 needs from us, and nothing else.
payload="$OUT/payload"
rm -rf "$payload"; mkdir -p "$payload"
cp "$OUT/images/guest-vmlinux" "$OUT/images/initrd.cpio.gz" "$payload/"

mkdir -p "$OUT/logs"
log_file="$OUT/logs/l1-$suite.log"

log "booting L1 (PVM host), suite=$suite"
set +e
timeout --foreground -k 5 "$((BOOT_TIMEOUT * 3))" \
	"$QEMU" \
	-machine q35,accel=kvm \
	-cpu host \
	-smp "$L1_CPUS" -m "$L1_MEM" \
	-kernel "$OUT/images/host-bzImage" \
	-drive file="$OUT/images/l1-rootfs.ext4",if=virtio,format=raw \
	-append "root=/dev/vda rw console=ttyS0,115200 panic=-1 pvmtest.suite=$suite" \
	-virtfs local,path="$payload",mount_tag=payload,security_model=none,readonly=on \
	-nographic -no-reboot -display none -serial mon:stdio \
	< /dev/null 2>&1 | tee "$log_file"
rc=${PIPESTATUS[0]}
set -e

# Written as an if: "a || b && die" is (a||b) && die, whose status is 1
# when the boot did *not* time out, and under set -e that exits here on
# every successful run.
if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
	die "L1 timed out -- see $log_file"
fi

if grep -q '^G: PVMTEST-RESULT: ok ' "$log_file"; then
	log "PVM guest: $(grep -m1 '^G: PVMTEST-RESULT:' "$log_file")"
	exit 0
fi
warn "PVM guest did not pass; last of the log:"
tail -40 "$log_file" >&2
exit 1
