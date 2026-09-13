#!/usr/bin/env bash
#
# modarg-ab.sh <name> <args-A> <args-B>
#
# A/B of one kvm-pvm module parameter on one host kernel: the same binary
# booted with MOD_ARGS=<args-A> and MOD_ARGS=<args-B>, so nothing but the
# parameter differs.  Each repetition runs both, in alternating order (A B,
# then B A), which puts slow drift on both sides.  Every boot runs the perf
# suite and then, one boot each, the Scaling cases named by AB_SCALING.
#
#   AB_CPUS     vCPU counts          (default "1 8")
#   AB_REPS     repetitions          (default 5)
#   AB_SCALING  Scaling cases, exact names (default "perf/fault-scaling
#               perf/fault-rounds"; empty: none)
#
# Logs: out/logs/l1-perf-pvm-ab-<name>-<A|B>-cpuN-rM[-<case>].log
# Result: out/perf/ab-<name>.tsv, medians per metric, cpus and side.
#
#   scripts/modarg-ab.sh direct-pf direct_pf=0 direct_pf=1

source "$(dirname "$0")/lib.sh"

NAME="${1:?usage: modarg-ab.sh <name> <args-A> <args-B>}"
ARGS_A="${2:?}"
ARGS_B="${3:?}"
CPUS="${AB_CPUS:-1 8}"
REPS="${AB_REPS:-5}"
SCALING="${AB_SCALING-perf/fault-scaling perf/fault-rounds}"

grep -q '^CONFIG_KVM_PVM_STATS=y' "$OUT/build-host/.config" 2>/dev/null &&
	die "host kernel built with CONFIG_KVM_PVM_STATS: not a timing build"

mkdir -p "$OUT/perf"
raw="$OUT/perf/ab-$NAME.raw"
: > "$raw"

one() { # side args n rep
	local side=$1 args=$2 n=$3 rep=$4 l
	l="$OUT/logs/l1-perf-pvm-ab-$NAME-$side-cpu$n-r$rep.log"
	MOD_ARGS="$args" GUEST_CPUS="$n" LOG_SUFFIX="ab-$NAME-$side-cpu$n-r$rep" \
		"$TESTBED/scripts/run-l1.sh" perf pvm >/dev/null 2>&1 ||
		warn "perf $side cpu$n r$rep: run-l1 failed"
	collect "$l" "$side" "$n"
	local c
	for c in $SCALING; do
		# Exact names run whatever suite they are in, and under "perf"
		# the boot is the quiet one the perf cases get.
		l="$OUT/logs/l1-perf-pvm-ab-$NAME-$side-cpu$n-r$rep-${c//\//-}.log"
		MOD_ARGS="$args" GUEST_CPUS="$n" GUEST_APPEND="pvmtest.only=$c" \
			LOG_SUFFIX="ab-$NAME-$side-cpu$n-r$rep-${c//\//-}" \
			"$TESTBED/scripts/run-l1.sh" perf pvm >/dev/null 2>&1 ||
			warn "$c $side cpu$n r$rep: run-l1 failed"
		collect "$l" "$side" "$n"
	done
}

collect() { # log side n
	if grep -aq '^\(G\[[a-z0-9]*\]: \)\?not ok' "$1"; then
		warn "$1: a case failed; metrics dropped"
		return
	fi
	sed -n 's/^\(G\[[a-z0-9]*\]: \)\?PVMTEST-METRIC: //p' "$1" | tr -d '\r' |
		awk -v n="$3" -v s="$2" '$NF == "#END" {print $1"\t"n"\t"s"\t"$2}' >> "$raw"
}

for rep in $(seq 1 "$REPS"); do
	for n in $CPUS; do
		log "rep $rep/$REPS cpus=$n"
		if [ $((rep % 2)) = 1 ]; then
			one A "$ARGS_A" "$n" "$rep"; one B "$ARGS_B" "$n" "$rep"
		else
			one B "$ARGS_B" "$n" "$rep"; one A "$ARGS_A" "$n" "$rep"
		fi
	done
done

sort "$raw" | awk -F'\t' '
{ k = $1 "\t" $2; v[k, $3] = v[k, $3] " " $4; keys[k] = 1 }
function med(s,   a, n, i, j, t) {
	n = split(s, a, " ")
	for (i = 2; i <= n; i++) for (j = i; j > 1 && a[j] + 0 < a[j-1] + 0; j--) { t = a[j]; a[j] = a[j-1]; a[j-1] = t }
	return n ? ((n % 2) ? a[(n+1)/2] : (a[n/2] + a[n/2+1]) / 2) : ""
}
END {
	printf "metric\tcpus\tA\tB\tdelta\tnA\tnB\n"
	for (k in keys) {
		a = med(v[k, "A"]); b = med(v[k, "B"])
		printf "%s\t%s\t%s\t%s\t%s\t%d\t%d\n", k, a, b,
			(a != "" && a + 0 != 0 && b != "") ? sprintf("%+.1f%%", 100 * (b - a) / a) : "",
			split(v[k, "A"], x, " "), split(v[k, "B"], y, " ")
	}
}' | sort > "$OUT/perf/ab-$NAME.tsv"
column -t -s "$(printf '\t')" "$OUT/perf/ab-$NAME.tsv"
