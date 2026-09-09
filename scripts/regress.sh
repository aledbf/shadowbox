#!/usr/bin/env bash
#
# The checks that have to pass before anything riskier is worth running.
#
# All of it happens on L0 under ordinary KVM, so none of it can wedge
# anything: the kernels under test run as guests of this machine's own
# stock kernel, which none of this code touches.
#
#   1. the guest kernel still boots through the PVH ELF note
#   2. it still boots as a bzImage
#   3. it still passes the default suite
#   4. the host kernel still boots to user space
#
# Point 4 is the one that catches a change to the PVM host backend
# breaking the host itself, which is the failure that would otherwise show
# up as an unexplained hang inside L1.

source "$(dirname "$0")/lib.sh"
check_run_deps

fail=0
step() {
	local name="$1"; shift
	printf '\n\033[1m--- %s\033[0m\n' "$name" >&2
	if "$@"; then
		printf '\033[32mPASS\033[0m %s\n' "$name" >&2
	else
		printf '\033[31mFAIL\033[0m %s\n' "$name" >&2
		fail=1
	fi
}

step "guest / PVH / smoke"     "$TESTBED/scripts/run-guest.sh" --boot pvh     --suite smoke   --name regress-pvh-smoke
step "guest / bzImage / smoke" "$TESTBED/scripts/run-guest.sh" --boot bzimage --suite smoke   --name regress-bzimage-smoke
step "guest / PVH / default"   "$TESTBED/scripts/run-guest.sh" --boot pvh     --suite default --name regress-pvh-default

# The host kernel is booted with the same initrd.  It is not a PVM guest
# here -- it is the host image running as an ordinary KVM guest -- but
# reaching user space is exactly what we need to know.
step "host kernel boots" env \
	OUT="$OUT" "$TESTBED/scripts/run-host-selftest.sh"

if [ "$fail" = 0 ]; then
	printf '\n\033[32mall regression checks passed\033[0m\n' >&2
else
	printf '\n\033[31msome regression checks failed\033[0m\n' >&2
fi
exit $fail
