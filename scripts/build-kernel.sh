#!/usr/bin/env bash
#
# build-kernel.sh {guest|host}
#
# Builds one of the two test kernels out of tree, from defconfig plus the
# matching fragments.  Deliberately not reusing an existing build
# directory: the point of the harness is that the image it boots is the
# one its own configuration describes.

source "$(dirname "$0")/lib.sh"
check_build_deps

role="${1-}"
[ -n "$role" ] || die "usage: build-kernel.sh guest|host"
case "$role" in guest|host) ;; *) die "unknown role: $role" ;; esac

B="$OUT/build-$role"
mkdir -p "$B"

log "configuring $role kernel in $B (KSRC=$KSRC)"
make -C "$KSRC" O="$B" -s defconfig
"$KSRC/scripts/kconfig/merge_config.sh" -m -O "$B" \
	"$B/.config" \
	"$TESTBED/configs/common.fragment" \
	"$TESTBED/configs/$role.fragment" >/dev/null
make -C "$KSRC" O="$B" -s olddefconfig

# merge_config.sh warns but does not fail when the kernel drops an option
# it was asked for.  For these two that is the difference between testing
# what we think we are testing and testing nothing, so check them.
want=(CONFIG_SERIAL_8250_CONSOLE=y)
case "$role" in
guest) want+=(CONFIG_PVM_GUEST=y CONFIG_X86_PIE=y CONFIG_PVH=y) ;;
host)  want+=(CONFIG_KVM_PVM=y) ;;
esac
for opt in "${want[@]}"; do
	grep -qx "$opt" "$B/.config" || die "$role: $opt did not survive olddefconfig"
done

log "building $role kernel with -j$JOBS"
make -C "$KSRC" O="$B" -j"$JOBS"

mkdir -p "$OUT/images"
cp "$B/arch/x86/boot/bzImage" "$OUT/images/$role-bzImage"
cp "$B/vmlinux"               "$OUT/images/$role-vmlinux"

if [ "$role" = host ]; then
	# L1 needs modules on its root filesystem.
	rm -rf "$OUT/modules-host"
	make -C "$KSRC" O="$B" -j"$JOBS" -s modules_install \
		INSTALL_MOD_PATH="$OUT/modules-host" INSTALL_MOD_STRIP=1
fi

log "$role kernel ready: $OUT/images/$role-bzImage"
