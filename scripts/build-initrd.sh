#!/usr/bin/env bash
#
# Builds the guest initrd: one static Go binary as /init, in a cpio
# archive.  Nothing else is in the image -- no shell, no libc, no
# busybox -- so a boot that reaches "PVMINIT" has genuinely reached user
# space rather than something a rescue shell papered over.

source "$(dirname "$0")/lib.sh"
need go; need cpio

STAGE="$OUT/initrd-root"
rm -rf "$STAGE"
mkdir -p "$STAGE"/{proc,sys,dev,tmp,run}

log "building the Go init (static, no cgo)"
( cd "$TESTBED/initrd" && \
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o "$STAGE/init" ./cmd/pvminit )

# /init is what the kernel execs; the second name is for a human poking
# around with rdinit=.
ln "$STAGE/init" "$STAGE/pvminit"

log "packing the cpio"
mkdir -p "$OUT/images"
( cd "$STAGE" && find . -print0 | cpio --null --create --format=newc --quiet ) \
	| gzip -9 > "$OUT/images/initrd.cpio.gz"

log "initrd ready: $OUT/images/initrd.cpio.gz ($(du -h "$OUT/images/initrd.cpio.gz" | cut -f1))"
