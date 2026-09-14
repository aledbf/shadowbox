# Shared settings and helpers.  Sourced, not executed.

set -Eeuo pipefail

TESTBED="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${OUT:-$TESTBED/out}"
CACHE="${CACHE:-$TESTBED/cache}"

# The kernel tree under test.  The testbed carries none: KSRC names the
# checkout the images in out/ are built from (the PVM series, or anything
# else), and batteries name their own trees with "kernel" directives.
KSRC="${KSRC:-}"
need_ksrc() {
	[ -n "$KSRC" ] && [ -d "$KSRC" ] ||
		die "KSRC must name a kernel tree (KSRC=${KSRC:-unset})"
}

JOBS="${JOBS:-$(nproc)}"
QEMU="${QEMU:-qemu-system-x86_64}"

# Guest sizing.  Small on purpose: a PVM boot failure should be a fast
# failure.
GUEST_CPUS="${GUEST_CPUS:-2}"
GUEST_MEM="${GUEST_MEM:-1G}"
L1_CPUS="${L1_CPUS:-8}"
L1_MEM="${L1_MEM:-8G}"

BOOT_TIMEOUT="${BOOT_TIMEOUT:-180}"

# Which CPUs to run a measured VM on.
#
# A hybrid CPU is the single largest source of run-to-run noise here: this
# machine has six P-cores at 5.2-5.4GHz and eight E-cores at 4.1GHz, and
# where the scheduler happens to put qemu's threads changes the answer by
# tens of percent.  Two sweeps of the same build read 14810 and 10020 ns
# on the same benchmark before this existed.
#
# So pin to the fastest set of CPUs, detected rather than hardcoded:
# whichever share the highest MAXMHZ.  On a uniform machine that is all of
# them and the pinning is a no-op.
#
#   PIN_CPUS=0-11   use exactly this list
#   PIN_CPUS=none   do not pin at all
pin_cpu_list() {
	if [ "${PIN_CPUS:-}" = none ]; then
		return 0
	fi
	if [ -n "${PIN_CPUS:-}" ]; then
		printf '%s' "$PIN_CPUS"
		return
	fi
	command -v lscpu >/dev/null 2>&1 || return
	lscpu -e=CPU,MAXMHZ 2>/dev/null | awk '
		NR > 1 && $2 != "" {
			mhz = $2 + 0
			cpu[NR] = $1
			f[NR] = mhz
			if (mhz > top) top = mhz
		}
		END {
			if (top == 0) exit
			sep = ""
			n = 0
			# 0.9 separates core *types*, not turbo bins: on this
			# machine the E-cores are at 76% of the top P-core and
			# the slower P-cores at 96%, so anything in between
			# would keep two cores and drop ten.
			for (i in f)
				if (f[i] >= top * 0.9) { list = list sep cpu[i]; sep = ","; n++ }
			# All of them means there is nothing to choose between.
			if (n > 1 && n < NR - 1) print list
		}'
}

# taskset prefix for a measured VM, empty when there is nothing to pin to.
pin_prefix() {
	local cpus
	cpus=$(pin_cpu_list)
	# return 0: callers run under set -e, and "nothing to pin" is not an error.
	[ -n "$cpus" ] || return 0
	command -v taskset >/dev/null 2>&1 || {
		warn "taskset missing: measurements will drift across core types"
		return
	}
	printf 'taskset -c %s' "$cpus"
}

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
