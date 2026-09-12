#!/usr/bin/env bash
#
# run-failclosed.sh
#
# The cases where kvm-pvm must refuse to load, checked by actually
# creating them rather than by reading hardware_cap_check().
#
# A refusal is only useful if it is complete: the module must not stay
# loaded, must leave no /dev/kvm behind, must say why in terms a person
# can act on, and must leave the host healthy.  A half-registration would
# be worse than a panic, because nothing would notice.
#
# What can actually be created here:
#
#   no-fsgsbase  boot L1 with clearcpuid=fsgsbase.  The switcher reads and
#                writes GSBASE on every ring switch, so a host without it
#                is refused; clearcpuid= is the only way to create the
#                condition on a CPU that has the feature.
#
# What cannot: FRED needs a CPU that has it, and this one does not.  There
# is no way to fake X86_FEATURE_FRED for the host kernel from here, so
# that arm of hardware_cap_check() is unexercised and says so below rather
# than being quietly skipped.
#
# What no longer belongs here: host KPTI.  It used to be refused; it is now
# supported and is covered positively by "make pti" instead.

source "$(dirname "$0")/lib.sh"

fail=0
check() {
	local name="$1" append="$2" want="$3"
	local log

	printf '\n\033[1m--- %s\033[0m\n' "$name" >&2
	L1_APPEND="$append" LOG_SUFFIX="$name" \
		"$TESTBED/scripts/run-l1.sh" failclosed pvm >/dev/null 2>&1 || true
	log="$OUT/logs/l1-failclosed-pvm-$name.log"
	if [ ! -f "$log" ]; then
		warn "$name: no log at $log"
		fail=1
		return
	fi

	if ! grep -q "^FAILCLOSED: ok " "$log"; then
		warn "$name: the module did not refuse:"
		grep -E "^FAILCLOSED:|L1: insmod:|L1: lsmod:" "$log" >&2 || tail -20 "$log" >&2
		fail=1
		return
	fi

	# It refused; now check it said something a person can act on.
	if ! grep -qi "$want" "$log"; then
		warn "$name: refused, but nothing in the log matches /$want/;"
		warn "        a refusal without a reason is a support ticket."
		grep "L1: dmesg:" "$log" | tail -10 >&2
		fail=1
		return
	fi

	log "$name: refused, and said so"
	grep -i "$want" "$log" | head -2 | sed 's/^/     /' >&2

	# And the host has to still be healthy.
	if ! "$TESTBED/scripts/sanitize-log.sh" -q "$log"; then
		warn "$name: refused cleanly but the L1 kernel log did not"
		fail=1
	fi
}

check no-fsgsbase "clearcpuid=fsgsbase" "FSGSBASE is required"

printf '\n' >&2
warn "not exercised: the FRED arm of hardware_cap_check() needs a CPU with"
warn "FRED, which this one does not have."

if [ "$fail" = 0 ]; then
	printf '\033[32mfail-closed checks passed\033[0m\n' >&2
else
	printf '\033[31mfail-closed checks failed\033[0m\n' >&2
fi
exit $fail
