# Shared settings and helpers.  Sourced, not executed.

set -Eeuo pipefail

TESTBED="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${OUT:-$TESTBED/out}"
CACHE="${CACHE:-$TESTBED/cache}"

# The kernel tree under test.  The 7.3 port lives here; override KSRC to
# point the same harness at the original 6.12 branch for an A/B run.
KSRC="${KSRC:-/home/aledbf/Trabajo/github/linux-aledbf}"

JOBS="${JOBS:-$(nproc)}"
QEMU="${QEMU:-qemu-system-x86_64}"

# Guest sizing.  Small on purpose: a PVM boot failure should be a fast
# failure.
GUEST_CPUS="${GUEST_CPUS:-2}"
GUEST_MEM="${GUEST_MEM:-1G}"
L1_CPUS="${L1_CPUS:-8}"
L1_MEM="${L1_MEM:-8G}"

BOOT_TIMEOUT="${BOOT_TIMEOUT:-180}"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[1;33m warn\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mfail\033[0m %s\n' "$*" >&2; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "missing tool: $1${2:+  ($2)}"
}

# Fail early and clearly rather than half way through a 10 minute build.
check_build_deps() {
	need make; need gcc; need ld; need bison; need flex; need bc
	need cpio; need go "the guest init is written in Go"
	command -v pahole >/dev/null 2>&1 || warn "pahole missing: BTF will be skipped"
}

check_run_deps() {
	command -v "$QEMU" >/dev/null 2>&1 || die \
		"missing $QEMU -- install qemu-system-x86 (Debian/Ubuntu: qemu-system-x86)"
	[ -w /dev/kvm ] || die "/dev/kvm is not writable by $(id -un): add yourself to the kvm group"
}
