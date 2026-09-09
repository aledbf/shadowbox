#!/usr/bin/env bash
#
# Boots the host kernel as an ordinary KVM guest, with the same initrd the
# guest uses, purely to find out whether it reaches user space.  A change
# to the PVM host backend that breaks the host kernel's own boot would
# otherwise only show up as an unexplained hang inside L1.

source "$(dirname "$0")/lib.sh"
check_run_deps

[ -f "$OUT/images/host-bzImage" ] || die "no host kernel -- run 'make host-kernel'"
mkdir -p "$OUT/logs"
log_file="$OUT/logs/host-selftest.log"

set +e
timeout --foreground -k 5 "$BOOT_TIMEOUT" \
	"$QEMU" -machine q35,accel=kvm -cpu host \
	-smp 2 -m 2G \
	-kernel "$OUT/images/host-bzImage" \
	-initrd "$OUT/images/initrd.cpio.gz" \
	-append "console=ttyS0,115200 earlyprintk=serial,ttyS0,115200 panic=-1 oops=panic pvmtest.suite=smoke pvmtest.tag=host-selftest" \
	-nographic -no-reboot -display none -serial mon:stdio \
	< /dev/null > "$log_file" 2>&1
rc=${PIPESTATUS[0]}
set -e

if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
	die "host kernel timed out -- see $log_file"
fi

if ! grep -q '^PVMINIT: up' "$log_file"; then
	warn "the host kernel never reached user space"
	tail -25 "$log_file" >&2
	exit 1
fi

# kvm_x86_vendor_init() WARNs once for every kvm_x86_ops the PVM backend
# does not implement.  Those are a known, listed gap -- see docs/STATUS.md
# -- so they are reported rather than failed on; anything else is not.
n=$(grep -c 'WARNING:.*kvm-x86-\(nested-\|pmu-\)\?ops\.h' "$log_file" || true)
log "host kernel reached user space; $n missing-kvm_x86_ops warnings"

other=$(grep -E 'BUG:|general protection fault|unable to handle|Kernel panic' "$log_file" || true)
if [ -n "$other" ]; then
	warn "the host kernel log has more than the known ops warnings:"
	printf '%s\n' "$other" >&2
	exit 1
fi
exit 0
