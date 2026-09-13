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

# HOST_CONFIG_EXTRA / GUEST_CONFIG_EXTRA: options on top of the fragments,
# space separated -- "CONFIG_KVM_PVM_STATS=y" for a counting build.  Every
# build states them, and a build without them drops them again, because the
# fragments are re-merged onto a fresh defconfig each time.
extra_var="$(echo "$role" | tr a-z A-Z)_CONFIG_EXTRA"
extra="${!extra_var:-}"
extra_frag="$B/.extra.fragment"
mkdir -p "$B"
: > "$extra_frag"
for opt in $extra; do
	case "$opt" in
	*=n) echo "# ${opt%=n} is not set" >> "$extra_frag" ;;
	*)   echo "$opt" >> "$extra_frag" ;;
	esac
done

log "configuring $role kernel in $B (KSRC=$KSRC)${extra:+ with $extra}"
make -C "$KSRC" O="$B" -s defconfig
"$KSRC/scripts/kconfig/merge_config.sh" -m -O "$B" \
	"$B/.config" \
	"$TESTBED/configs/common.fragment" \
	"$TESTBED/configs/$role.fragment" \
	"$extra_frag" >/dev/null
make -C "$KSRC" O="$B" -s olddefconfig

# merge_config.sh warns but does not fail when the kernel drops an option
# it was asked for.  For these two that is the difference between testing
# what we think we are testing and testing nothing, so check them.
want=(CONFIG_SERIAL_8250_CONSOLE=y)
case "$role" in
guest) want+=(CONFIG_PVM_GUEST=y CONFIG_X86_PIE=y CONFIG_PVH=y) ;;
host)  want+=(CONFIG_KVM_PVM=m CONFIG_KVM_INTEL=m) ;;
esac
for opt in $extra; do
	want+=("$opt")
done
for opt in "${want[@]}"; do
	# "CONFIG_FOO=n" is written back as "# CONFIG_FOO is not set".
	case "$opt" in
	*=n) line="# ${opt%=n} is not set" ;;
	*)   line="$opt" ;;
	esac
	grep -qx "$line" "$B/.config" || die "$role: $opt did not survive olddefconfig"
done

log "building $role kernel with -j$JOBS"
make -C "$KSRC" O="$B" -j"$JOBS"

mkdir -p "$OUT/images"
cp "$B/arch/x86/boot/bzImage" "$OUT/images/$role-bzImage"

# Two copies of the ELF image.  The full one carries the debug info and is
# what to point a debugger at; the one qemu boots has it stripped, because
# with CONFIG_DEBUG_INFO the guest vmlinux is around 450MB and every boot
# would read all of it.  --strip-debug leaves the Xen ELF notes alone, and
# XEN_ELFNOTE_PHYS32_ENTRY is how qemu -kernel finds pvh_start_xen(), so
# that is checked rather than assumed.
cp "$B/vmlinux" "$OUT/images/$role-vmlinux.debug"
objcopy --strip-debug "$B/vmlinux" "$OUT/images/$role-vmlinux"
if [ "$role" = guest ]; then
	readelf -n "$OUT/images/$role-vmlinux" | grep -q '0x00000012' ||
		die "the stripped guest image lost XEN_ELFNOTE_PHYS32_ENTRY"
fi

if [ "$role" = host ]; then
	# L1 needs modules on its root filesystem.
	rm -rf "$OUT/modules-host"
	make -C "$KSRC" O="$B" -j"$JOBS" -s modules_install \
		INSTALL_MOD_PATH="$OUT/modules-host" INSTALL_MOD_STRIP=1
fi

log "$role kernel ready: $OUT/images/$role-bzImage"
