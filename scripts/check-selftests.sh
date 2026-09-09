#!/usr/bin/env bash
#
# check-selftests.sh <log> <vendor>
#
# Compares the SELFTEST: lines an L1 run produced against what
# configs/kvm-selftests-expect.txt says that vendor should do, and fails
# on any difference -- in either direction.
#
# Something that starts failing is a regression.  Something that starts
# passing is a gap that closed, and the expectation should be updated
# deliberately rather than left saying the opposite.  Both are news, so
# both are reported.

source "$(dirname "$0")/lib.sh"

log_file="${1:?usage: check-selftests.sh <log> <vendor>}"
vendor="${2:?usage: check-selftests.sh <log> <vendor>}"
expect="$TESTBED/configs/kvm-selftests-expect.txt"

[ -f "$log_file" ] || die "no such log: $log_file"
[ -f "$expect" ]   || die "missing $expect"

fail=0
seen=0

# The log comes off a serial console, so strip the CRs before matching.
while read -r _ name verdict _; do
	seen=$((seen + 1))
	want=$(awk -v n="$name" -v v="$vendor" \
		'$1==n && $2==v { print $3; exit }' "$expect")
	if [ -z "$want" ]; then
		warn "$name: no expectation recorded for vendor=$vendor (got $verdict)"
		fail=1
		continue
	fi
	if [ "$verdict" != "$want" ]; then
		if [ "$want" = fail ] || [ "$want" = skip ]; then
			printf '\033[1;33mCHANGED\033[0m %-34s %s -> %s  (a gap may have closed; update configs/kvm-selftests-expect.txt)\n' \
				"$name" "$want" "$verdict" >&2
		else
			printf '\033[1;31mREGRESSED\033[0m %-32s %s -> %s\n' \
				"$name" "$want" "$verdict" >&2
		fi
		fail=1
	fi
done < <(tr -d '\r' < "$log_file" | grep '^SELFTEST: ')

[ "$seen" -gt 0 ] || die "the log has no SELFTEST: lines at all"

# A test that is listed but never ran is as much of a problem as one that
# changed verdict: it means the build or the payload dropped it.
while read -r name v _; do
	[ "$v" = "$vendor" ] || continue
	if ! tr -d '\r' < "$log_file" | grep -q "^SELFTEST: $name "; then
		warn "$name is expected for vendor=$vendor but did not run"
		fail=1
	fi
done < <(grep -vE '^[[:space:]]*(#|$)' "$expect")

if [ "$fail" = 0 ]; then
	log "selftests ($vendor): $seen tests, all as recorded"
else
	printf '\n\033[1;31mcheck-selftests: results differ from configs/kvm-selftests-expect.txt\033[0m\n' >&2
fi
exit $fail
