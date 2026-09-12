#!/usr/bin/env bash
#
# Turns the agent's "exitrip" lines into guest kernel symbols.
#
# The reason histogram says what the exits were; this says which guest code
# asked for them, which is the half you can act on.  The resolution has to
# happen here rather than in L1: the guest kernel is PIE and relocates
# itself into the lower half at boot, so a RIP from the trace is a link
# address plus an offset that only the running guest knows.  It prints that
# offset on every boot -- "PVMINIT: _text at ..." -- and vmlinux has the
# link address, so the difference is the delta.
#
# Addresses are compared as zero-padded 16-digit hex strings rather than as
# numbers: "nm -n" emits them that way already, lexicographic order is the
# same as numeric order for a fixed width, and the awk in a Debian rootfs is
# mawk, which has no strtonum().
#
# Usage: scripts/resolve-exits.sh <log> [vmlinux]

source "$(dirname "$0")/lib.sh"

LOG="${1:?usage: resolve-exits.sh <log> [vmlinux]}"
VMLINUX="${2:-$OUT/build-guest/vmlinux}"

[ -f "$LOG" ] || die "no such log: $LOG"
[ -f "$VMLINUX" ] || die "no guest vmlinux at $VMLINUX (build the guest kernel first)"
command -v nm >/dev/null || die "nm is not installed (binutils)"

# One process, no pipe: lib.sh sets pipefail, and a "| head -1" that closes
# the pipe early makes the producer fail and takes the script with it.
runtime=$(awk 'match($0, /PVMINIT: _text at 0x[0-9a-f]+/) {
		s = substr($0, RSTART, RLENGTH); sub(/.*0x/, "", s); print s; exit
	}' "$LOG")
[ -n "$runtime" ] || die "the log has no 'PVMINIT: _text at' line; was it a guest run?"

link=$(nm "$VMLINUX" | awk '$3 == "_text" { print $1 }')
[ -n "$link" ] || die "vmlinux has no _text symbol"

delta=$((16#$runtime - 16#$link))
log "guest _text: runtime 0x$runtime, link 0x$link, delta $delta"

syms=$(mktemp); trap 'rm -f "$syms"' EXIT
nm -n "$VMLINUX" | awk '$2 ~ /^[tTwW]$/ { printf "%s %s\n", tolower($1), $3 }' > "$syms"

found=0
# A reason can contain a space ("GP excp"), so take the count as the first
# field and the rip as the last, and whatever is between them as the reason.
while read -r count rest; do
	[ -n "${rest:-}" ] || continue
	rip="${rest##* }"
	reason="${rest% *}"
	reason="${reason%"${reason##*[![:space:]]}"}"
	want=$(printf '%016x' $(( rip - delta )))
	read -r base sym <<<"$(awk -v w="$want" '$1 <= w { b=$1; s=$2; next } { exit }
		END { print b, s }' "$syms")"
	if [ -n "$sym" ]; then
		printf '%8d  %-22s %-18s %s+0x%x\n' \
			"$count" "$reason" "$rip" "$sym" $(( 16#$want - 16#$base ))
		found=$((found + 1))
	else
		printf '%8d  %-22s %-18s %s\n' "$count" "$reason" "$rip" "(not in vmlinux)"
	fi
done < <(sed -n 's/^L1: exitrip: *//p' "$LOG" | tr -d '\r')

[ "$found" -gt 0 ] || die "no exitrip lines resolved -- is this a 'make mmu' log?"
