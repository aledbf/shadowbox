#!/usr/bin/env bash
#
# Where a benchmark's exits go, attributed to the benchmark.
#
# "make mmu" counts events over the whole L1 run, which is boot, the case,
# and teardown.  For a case that runs for a second inside a three second
# run that is not an attribution: PVM boots by emulating instructions one
# at a time until the guest reaches long mode, and that alone can outweigh
# whatever the case did.
#
# So run it twice per vendor -- once with a case filter that matches
# nothing, which boots and tears down and runs no case at all, and once
# with the case -- and subtract.  What is left is the case.
#
# Usage: scripts/exit-profile.sh <case> [vendor...]
#   scripts/exit-profile.sh perf/context-switch
#   scripts/exit-profile.sh perf/syscall pvm

source "$(dirname "$0")/lib.sh"

CASE="${1:?usage: exit-profile.sh <case> [vendor...]}"
shift
VENDORS=("$@")
[ ${#VENDORS[@]} -eq 0 ] && VENDORS=(pvm intel)

# A filter no case name contains, so the guest boots and runs nothing.
NO_CASE="__baseline_no_case__"

counts() { # <logfile> -> "event count" lines
	sed -n 's/^L1: mmu: *\([0-9][0-9]*\) *\([a-z_]*:[a-z_]*\).*/\2 \1/p' "$1"
	# A CONFIG_KVM_PVM_STATS host prints one PVMSTATS line per vCPU as the
	# guest is torn down; sum them, in the order they were printed.
	tr -d '\r' < "$1" | sed -n 's/.*PVMSTATS: vcpu=[0-9]* //p' | tr ' ' '\n' |
		awk -F= 'NF == 2 {
			if (!($1 in sum)) order[++n] = $1
			sum[$1] += $2
		}
		END { for (i = 1; i <= n; i++) print "pvmstat:" order[i], sum[order[i]] }'
}

run() { # <vendor> <only> <logfile>
	GUEST_APPEND="pvmtest.only=$2" timeout 400 "$TESTBED/scripts/run-l1.sh" mmu "$1" \
		> "$3" 2>&1 || true
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

for v in "${VENDORS[@]}"; do
	log "$v: baseline (boot and teardown, no case)"
	run "$v" "$NO_CASE" "$tmp/$v.base"
	log "$v: $CASE"
	run "$v" "$CASE" "$tmp/$v.case"

	counts "$tmp/$v.base" | sort > "$tmp/$v.base.c"
	counts "$tmp/$v.case" | sort > "$tmp/$v.case.c"

	if [ ! -s "$tmp/$v.case.c" ]; then
		echo "$v: no counters in the run -- did perf stat work?" >&2
		grep -E "PVMTEST-RESULT|insmod" "$tmp/$v.case" >&2 || true
		continue
	fi
	# A case that did not run makes the subtraction meaningless.
	grep -q "ok 1 - $CASE" "$tmp/$v.case" ||
		echo "$v: WARNING: '$CASE' did not report a pass; the delta is not the case" >&2
done

printf '\n%-28s' "event"
for v in "${VENDORS[@]}"; do printf '%14s' "$v"; done
[ ${#VENDORS[@]} -eq 2 ] && printf '%10s' "ratio"
printf '\n%-28s' "----------------------------"
for v in "${VENDORS[@]}"; do printf '%14s' "-------------"; done
[ ${#VENDORS[@]} -eq 2 ] && printf '%10s' "---------"
echo

# The union of event names, in the order perf listed them for the first
# vendor, so kvm_exit stays at the top where it belongs.
events="$(counts "$tmp/${VENDORS[0]}.case" | awk '{print $1}')"
for e in $events; do
	printf '%-28s' "$e"
	a=""; b=""
	for v in "${VENDORS[@]}"; do
		base=$(awk -v e="$e" '$1==e{print $2}' "$tmp/$v.base.c")
		case_=$(awk -v e="$e" '$1==e{print $2}' "$tmp/$v.case.c")
		d=$(( ${case_:-0} - ${base:-0} ))
		printf '%14s' "$d"
		[ -z "$a" ] && a="$d" || b="$d"
	done
	if [ ${#VENDORS[@]} -eq 2 ]; then
		# a is the first vendor, b the second, and the ratio is the
		# first over the second: "pvm intel" asks how much worse PVM
		# is, which is the question.
		printf '%10s' "$(awk -v a="$a" -v b="$b" \
			'BEGIN{ if (b+0<=0) print "-"; else printf "%.1fx", a/b }')"
	fi
	echo
done

cat <<'EOT'

Deltas: the case minus a run of the same guest with no case selected.
A negative number is noise -- boot and teardown are not identical between
two runs -- and says the case did not move that counter.
EOT
