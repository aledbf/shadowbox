#!/usr/bin/env bash
#
# run-guest.sh [--boot pvh|bzimage] [--suite NAME] [--name TAG] [-- extra qemu args]
#
# Boots the guest kernel with the Go initrd and returns the harness's
# verdict.  Used twice: on L0 against ordinary KVM (stage 0), and inside
# L1 against kvm-pvm (stage 2).  The command line is the same in both
# cases on purpose -- if the two disagree, the difference is PVM.

source "$(dirname "$0")/lib.sh"
check_run_deps

boot=pvh
suite=default
tag=""
while [ $# -gt 0 ]; do
	case "$1" in
	--boot)  boot="$2"; shift 2 ;;
	--suite) suite="$2"; shift 2 ;;
	--name)  tag="$2"; shift 2 ;;
	--) shift; break ;;
	*) die "unknown argument: $1" ;;
	esac
done
tag="${tag:-guest-$boot-$suite}"

case "$boot" in
pvh)     kernel="$OUT/images/guest-vmlinux" ;;   # entered at the PVH ELF note
bzimage) kernel="$OUT/images/guest-bzImage" ;;
*) die "unknown boot mode: $boot" ;;
esac
[ -f "$kernel" ] || die "no $kernel -- run 'make guest-kernel' first"
[ -f "$OUT/images/initrd.cpio.gz" ] || die "no initrd -- run 'make initrd' first"

mkdir -p "$OUT/logs"
log_file="$OUT/logs/$tag.log"

cmdline="console=ttyS0,115200 earlyprintk=serial,ttyS0,115200 panic=-1"
cmdline="$cmdline oops=panic nokaslr no_timer_check"
cmdline="$cmdline pvmtest.suite=$suite pvmtest.tag=$tag"

log "booting $tag  ($(basename "$kernel"))"
set +e
timeout --foreground -k 5 "$BOOT_TIMEOUT" \
	"$QEMU" \
	-machine q35,accel=kvm \
	-cpu host \
	-smp "$GUEST_CPUS" -m "$GUEST_MEM" \
	-kernel "$kernel" \
	-initrd "$OUT/images/initrd.cpio.gz" \
	-append "$cmdline" \
	-nographic -no-reboot \
	-serial mon:stdio \
	-display none \
	"$@" 2>&1 | tee "$log_file"
rc=${PIPESTATUS[0]}
set -e

if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
	die "$tag: timed out after ${BOOT_TIMEOUT}s -- see $log_file"
fi

# The harness prints exactly one of these as its last act.  Anything else
# means the guest died before it could report, which is the failure mode
# that matters most here.
if grep -q '^PVMTEST-RESULT: ok ' "$log_file"; then
	log "$tag: $(grep -m1 '^PVMTEST-RESULT:' "$log_file")"
	exit 0
fi
if grep -q '^PVMTEST-RESULT: fail ' "$log_file"; then
	warn "$tag: $(grep -m1 '^PVMTEST-RESULT:' "$log_file")"
	grep '^not ok ' "$log_file" >&2 || true
	exit 1
fi

warn "$tag: the guest never reported a result"
tail -30 "$log_file" >&2
exit 2
