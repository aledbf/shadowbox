#!/usr/bin/env bash
#
# Builds the subset of the kernel's own KVM selftests listed in
# configs/kvm-selftests.txt and stages it for the L1 payload.
#
# Two things about this build are not obvious:
#
#   - It needs the tree's own uapi headers.  The selftests reach for
#     KHDR_INCLUDES, which on a distro points at /usr/include, and that
#     asm/kvm.h is older than the tests: the build fails on
#     KVM_X86_QUIRK_NESTED_SVM_SHARED_PAT before it gets anywhere.
#   - It does not honour O= for the final link, so the binaries land in
#     the source tree.  They are gitignored there, so this leaves them
#     alone rather than fighting the kernel's own Makefile.

source "$(dirname "$0")/lib.sh"
need_ksrc
need gcc

list="$TESTBED/configs/kvm-selftests.txt"
[ -f "$list" ] || die "missing $list"

B="$OUT/build-host"
hdr="$B/usr/include"
src="$KSRC/tools/testing/selftests/kvm"

if [ ! -f "$hdr/linux/kvm.h" ]; then
	log "installing uapi headers from $KSRC"
	make -C "$KSRC" O="$B" headers_install INSTALL_HDR_PATH="$B/usr" -s ||
		die "headers_install failed"
fi

mapfile -t tests < <(grep -vE '^[[:space:]]*(#|$)' "$list")
[ ${#tests[@]} -gt 0 ] || die "no tests listed in $list"

log "building ${#tests[@]} selftests (this takes a few minutes the first time)"
# Absolute targets.  The KVM selftests Makefile's link rules are written for
# $(OUTPUT)/<test>, OUTPUT being the absolute directory; a relative
# "x86/foo" misses them and falls through to make's builtin %: %.o, which
# links without libkvm.  That only shows once a test's source changes -- until
# then the binary is up to date and nothing is linked at all.
make -C "$src" ARCH=x86 KHDR_INCLUDES="-isystem $hdr" -j"$JOBS" \
	"${tests[@]/#/$src/}" > "$OUT/kvm-selftests-build.log" 2>&1 ||
	{ tail -20 "$OUT/kvm-selftests-build.log" >&2
	  die "selftest build failed -- see out/kvm-selftests-build.log"; }

stage="$OUT/kvm-selftests"
rm -rf "$stage"
mkdir -p "$stage"
for t in "${tests[@]}"; do
	[ -x "$src/$t" ] || die "$t did not build"
	# Flattened, with the directory folded into the name, so the agent
	# can just iterate over one directory.
	cp "$src/$t" "$stage/${t//\//__}"
done

log "staged $(ls "$stage" | wc -l) selftests ($(du -sh "$stage" | cut -f1))"
