#!/usr/bin/env bash
#
# build-ref.sh <name> <git-dir> <rev> [timing|stats]
#
# A complete set of host and guest images built from one revision of a git
# tree, in out/refs/<name>/, for comparing kernels side by side -- upstream
# with kvm-intel against the PVM series with kvm-pvm.  run-l1.sh boots a
# set with OUT=out/refs/<name>; tools/pvmtest does that for items with
# kernel=<name>.
#
# The source is a detached worktree of <git-dir> at out/refs/<name>/src: no
# branch is created, and <git-dir>'s own checkout is not touched.  The
# rootfs, initrd, host tests and KVM selftests are the testbed's, linked in.
# Nothing is rebuilt when the revision and the variant are the ones already
# built.

source "$(dirname "$0")/lib.sh"

name="${1:?usage: build-ref.sh <name> <git-dir> <rev> [timing|stats]}"
git_dir="${2:?}"
rev="${3:?}"
variant="${4:-timing}"
case "$variant" in timing|stats) ;; *) die "variant: timing or stats" ;; esac

ref_out="$OUT/refs/$name"
src="$ref_out/src"
sha=$(git -C "$git_dir" rev-parse --verify "$rev^{commit}") || die "$git_dir: no revision $rev"
stamp="$sha $variant"

mkdir -p "$ref_out/images"
if [ -f "$ref_out/built" ] && [ "$(cat "$ref_out/built")" = "$stamp" ] &&
   [ -f "$ref_out/images/host-bzImage" ] && [ -f "$ref_out/images/guest-vmlinux" ]; then
	log "$name: $rev ($(git -C "$git_dir" log -1 --format=%h "$sha")) $variant already built"
else
	if [ -d "$src" ]; then
		git -C "$src" checkout -q --detach "$sha"
	else
		git -C "$git_dir" worktree add -q --detach "$src" "$sha"
	fi
	extra=""
	[ "$variant" = stats ] && extra="CONFIG_KVM_PVM_STATS=y"
	[ -d "$src/arch/x86/kvm/pvm" ] || extra=""
	rm -f "$ref_out/built"
	OUT="$ref_out" KSRC="$src" "$TESTBED/scripts/build-kernel.sh" guest
	OUT="$ref_out" KSRC="$src" HOST_CONFIG_EXTRA="$extra" "$TESTBED/scripts/build-kernel.sh" host
	echo "$stamp" > "$ref_out/built"
fi

# What every set shares with the testbed's own.
for f in images/l1-rootfs.ext4 images/initrd.cpio.gz hosttests kvm-selftests; do
	[ -e "$OUT/$f" ] && ln -sfn "$OUT/$f" "$ref_out/$f"
done
log "$name: images in $ref_out/images"
