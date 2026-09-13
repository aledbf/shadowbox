#!/usr/bin/env bash
#
# case-stats.sh <l1 log> [counter-regex]
#
# Per-case counter deltas from a run with pvmtest.statsmsr set, on a host
# built with CONFIG_KVM_PVM_STATS.
#
# The guest writes MSR_PVM_STATS_MARK just before and just after something --
# the harness does it around every case (2n before case n, 2n+1 after), and a
# case may bracket its own phases the same way with an even value and the odd
# one after it -- and prints what each value stands for as PVMTEST-MARK.  The
# host answers each write by printing every vCPU's counters and the VM's,
# tagged mark=<value>.  The delta between the two prints of a pair is that
# phase alone: not the boot, which is where 38k of a suite's 47k single-page
# TLB flushes turned out to be, and not a second case-less run subtracted
# from the first.
#
# One column per pair, in mark order, one row per counter, vCPUs summed.  The
# counter regex narrows the rows (default: all).
#
#   GUEST_APPEND=pvmtest.statsmsr=0x4b564d2f LOG_SUFFIX=marks \
#       scripts/run-l1.sh perf pvm
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
	if (v % 2 == 0) label[v] = name
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
	# The pairs, in mark order: an even value with its odd successor.
	np = 0
	for (v in label) if ((v in have) && ((v + 1) in have)) pairs[++np] = v + 0
	for (i = 2; i <= np; i++)
		for (j = i; j > 1 && pairs[j] < pairs[j-1]; j--) { t = pairs[j]; pairs[j] = pairs[j-1]; pairs[j-1] = t }
	if (!np) { print "no complete mark pairs: was pvmtest.statsmsr set on a counting host?" > "/dev/stderr"; exit 1 }
	printf "%-30s", "counter"
	for (c = 1; c <= np; c++) {
		n = label[pairs[c]]; sub(/^perf\//, "", n)
		printf " %16s", substr(n, length(n) > 16 ? length(n) - 15 : 1)
	}
	printf "\n"
	for (k = 1; k <= nkeys; k++) {
		key = order[k]
		printf "%-30s", key
		for (c = 1; c <= np; c++) {
			b = pairs[c]
			printf " %16d", val[b + 1, key] - val[b, key]
		}
		printf "\n"
	}
}'
