#!/usr/bin/env bash
#
# Builds the host-side tests -- the ones that drive the KVM API directly,
# from inside L1, rather than running inside the guest.
#
# Plain gcc against the tree's own uapi headers.  Not the KVM selftests
# library: it builds a guest that expects to run at CPL0 with its own page
# tables, which is exactly what a PVM VM does not give it.  What is being
# tested here is the VMM-facing side -- the PVM MSRs, the PVCS pinning,
# the memslot lifetime around it -- and none of that needs the guest to
# execute anything.

source "$(dirname "$0")/lib.sh"
need_ksrc
need gcc

B="$OUT/build-host"
hdr="$B/usr/include"

# The uapi headers, installed from the tree under test rather than from
# whatever the build machine's libc ships.
#
# Every time, not just when they are missing.  The tests include the tree's
# <asm/pvm_para.h> for the PVM ABI, so a stale copy here is a copy of an ABI
# the kernel under test no longer implements -- which is a test failure that
# looks like a kernel bug.
log "installing uapi headers from $KSRC"
make -C "$KSRC" O="$B" headers_install INSTALL_HDR_PATH="$B/usr" -s ||
	die "headers_install failed"

mkdir -p "$OUT/hosttests"
for src in "$TESTBED"/hosttests/*.c; do
	name=$(basename "$src" .c)
	log "building $name"
	gcc -Wall -Wextra -Wno-unused-parameter -O2 -g \
		-isystem "$hdr" \
		-o "$OUT/hosttests/$name" "$src" -lpthread ||
		die "building $name failed"
done

log "host tests ready: $(ls "$OUT/hosttests" | tr '\n' ' ')"
