#!/usr/bin/env bash
#
# case-stats.sh <l1 log> [counter-regex]
#
# Per-case counter deltas from a run with pvmtest.statsmsr set, on a host
# built with CONFIG_KVM_PVM_STATS.
#
# The guest writes MSR_PVM_STATS_MARK just before case n (value 2n) and just
# after it (2n+1), and prints which case each value stands for as
# PVMTEST-MARK.  The host answers each write by printing every vCPU's
# counters and the VM's, tagged mark=<value>.  The delta between the two
# prints is the case alone -- which is what exit-profile.sh approximates by
# subtracting a second, case-less boot, and what the whole-run counters do
# not give at all: boot is in them, and boot is where 38k of the suite's 47k
# single-page TLB flushes turned out to be.
#
# One column per case, one row per counter, vCPUs summed.  The counter regex
# narrows the rows (default: all).
#
#   L1_APPEND= GUEST_APPEND=pvmtest.statsmsr=0x4b564d2f \
#       LOG_SUFFIX=marks scripts/run-l1.sh perf pvm
#   scripts/case-stats.sh out/logs/l1-perf-pvm-marks.log 'pf_|hc_wrmsr|exits'

source "$(dirname "$0")/lib.sh"

LOG="${1:?usage: case-stats.sh <log> [counter-regex]}"
FILTER="${2:-.}"
[ -f "$LOG" ] || die "no such log: $LOG"

# The console copy only: the agent echoes dmesg back under "L1: " prefixes.
tr -d '\r' < "$LOG" | awk -v filter="$FILTER" '
/PVMTEST-MARK: [0-9]+ [^ ]+ #END/ {
	sub(/.*PVMTEST-MARK: /, "")
	v = $1; name = $2
	sub(/\/(before|after)$/, "", name)
	if (v % 2 == 0) { casename[v / 2] = name; if (v / 2 > ncase) ncase = v / 2 }
	next
}
/^L1: / { next }
/PVMSTATS(-VM)?: mark=[0-9]+ / {
	vm = ($0 ~ /PVMSTATS-VM:/)
	sub(/.*PVMSTATS(-VM)?: /, "")
	split($1, m, "="); mark = m[2]
	for (i = 2; i <= NF; i++) {
		if ($i ~ /^vcpu=/) continue
		split($i, kv, "=")
		if (kv[1] !~ filter) continue
		key = (vm ? "vm." : "") kv[1]
		if (!(key in seen)) { seen[key] = 1; order[++nkeys] = key }
		val[mark, key] += kv[2]
		have[mark] = 1
	}
}
END {
	if (!ncase) { print "no PVMTEST-MARK lines: was pvmtest.statsmsr set?" > "/dev/stderr"; exit 1 }
	printf "%-30s", "counter"
	for (c = 1; c <= ncase; c++) {
		n = casename[c]; sub(/^[^\/]*\//, "", n)
		printf " %14s", substr(n, 1, 14)
	}
	printf "\n"
	for (k = 1; k <= nkeys; k++) {
		key = order[k]
		printf "%-30s", key
		for (c = 1; c <= ncase; c++) {
			b = 2 * c; a = 2 * c + 1
			if (!(b in have) || !(a in have)) { printf " %14s", "-"; continue }
			printf " %14d", val[a, key] - val[b, key]
		}
		printf "\n"
	}
}'
