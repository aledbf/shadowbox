#!/usr/bin/env bash
#
# Boots L1 -- the PVM host -- as an ordinary KVM guest of this machine,
# with the guest kernel handed in over 9p.  L1 needs no nested VMX: a PVM
# host uses none, which is the property this whole harness exists to
# check.

source "$(dirname "$0")/lib.sh"
check_run_deps

suite="${1:-default}"
# Which KVM vendor L1 should load.  "pvm" is the point of the exercise;
# "intel" runs the same guest under ordinary nested KVM, which is the only
# honest baseline for a perf comparison.
vendor="${2:-pvm}"
[ -f "$OUT/images/host-bzImage" ]   || die "no host kernel -- run 'make host-kernel'"
[ -f "$OUT/images/l1-rootfs.ext4" ] || die "no L1 rootfs -- run 'make rootfs'"

# What L1 needs from us, and nothing else.  A fresh directory each run, and
# gone afterwards: a fixed path under out/ that gets rm -rf'd at the start
# is one sudo run away from breaking every unprivileged run after it.
payload="$(mktemp -d "${TMPDIR:-/tmp}/pvm-payload.XXXXXX")"
trap 'rm -rf "$payload"' EXIT
cp "$OUT/images/guest-vmlinux" "$OUT/images/initrd.cpio.gz" "$payload/"
install -m 0755 "$TESTBED/scripts/l1-agent.sh" "$payload/agent.sh"

# The KVM modules travel with the payload rather than in the image: the
# rootfs is bootstrapped once, and the kernel's version string -- and so
# the path modprobe would look under -- changes with every commit.
find "$OUT/modules-host" -name 'kvm*.ko*' -exec cp {} "$payload/" \; 2>/dev/null || true

# perf, if one has been built against this tree.  L1's own distro perf
# would not know the PVM exit reasons.
# A lean perf: "perf kvm stat" needs libtraceevent, and libdw makes its
# symbols readable, but the full-featured build drags in python, slang,
# capstone and curl, none of which a minbase L1 has.
PERF="${PERF:-$TESTBED/../build-perf-lean/perf}"
if [ -x "$PERF" ]; then
	install -m 0755 "$PERF" "$payload/perf"
	# The two libraries L1 does not carry, alongside it.
	for lib in libtraceevent.so.1 libdw.so.1; do
		src=$(ldconfig -p | awk -v n="$lib" '$1==n{print $NF; exit}')
		[ -n "$src" ] && cp "$src" "$payload/"
	done
fi

mkdir -p "$OUT/logs"
log_file="$OUT/logs/l1-$suite${2:+-$vendor}.log"

log "booting L1 (KVM vendor=$vendor), suite=$suite"
set +e
case "$suite" in
full|perf|all) l1_timeout=2400 ;;
*)             l1_timeout=$((BOOT_TIMEOUT * 3)) ;;
esac

timeout --foreground -k 5 "$l1_timeout" \
	"$QEMU" \
	-machine q35,accel=kvm \
	-cpu host \
	-smp "$L1_CPUS" -m "$L1_MEM" \
	-kernel "$OUT/images/host-bzImage" \
	-drive file="$OUT/images/l1-rootfs.ext4",if=virtio,format=raw \
	-append "root=/dev/vda rw console=ttyS0,115200 panic=-1 pvmtest.suite=$suite pvmtest.vendor=$vendor \
systemd.mask=serial-getty@ttyS0.service systemd.show_status=false" \
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

# The agent tags each guest's output with the machine type it booted, so
# the prefix is "G[q35]:" rather than "G:".
if grep -q '^G\[[a-z0-9]*\]: PVMTEST-RESULT: ok ' "$log_file"; then
	log "PVM guest: $(grep -m1 'PVMTEST-RESULT:' "$log_file" | sed 's/.*PVMTEST/PVMTEST/')"
	exit 0
fi
warn "PVM guest did not pass; last of the log:"
tail -40 "$log_file" >&2
exit 1
