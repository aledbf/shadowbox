#!/usr/bin/env bash
#
# Puts the PVM numbers next to the plain-KVM ones.  Reads the metric
# lines the guest printed; no thresholds, because the only useful
# comparison is against the same guest on the same machine.

source "$(dirname "$0")/lib.sh"

# Three runs, because two of them answer different questions.
#
# L1/intel is the one that matters: the same guest, the same machine, the
# same number of virtualisation layers, under ordinary nested KVM instead
# of PVM.  Comparing PVM inside L1 against KVM on the bare machine would
# be measuring the nesting, not PVM.
l0="$OUT/logs/perf-kvm.log"
nested="$OUT/logs/l1-perf-intel.log"
pvm="$OUT/logs/l1-perf-pvm.log"
for f in "$nested" "$pvm"; do
	[ -f "$f" ] || die "missing $(basename "$f") -- run 'make perf'"
done

# The guest's own lines are unprefixed on L0 and carry the agent's
# "G[<machine>]: " tag when they came out of L1.
#
# The "#END" marker has to be there: these arrive over the same serial
# console the kernel prints to, and a printk landing mid-line leaves a
# truncated name and a fragment of a timestamp where the value was.  Taking
# that at face value is how a 4.8x context-switch regression was reported as
# 0.00x.  Lines without the marker are counted and reported, not used.
metrics() {
	sed -n 's/^\(G\[[a-z0-9]*\]: \)\?PVMTEST-METRIC: //p' "$1" | tr -d '\r' |
		awk '$NF == "#END" { print $1, $2 }' | sort
}

truncated() {
	sed -n 's/^\(G\[[a-z0-9]*\]: \)\?PVMTEST-METRIC: //p' "$1" | tr -d '\r' |
		awk '$NF != "#END"' | sed "s|^|$(basename "$1"): |"
}

fmt() { awk -v v="$1" 'BEGIN{ if (v=="") print "-"; else printf "%.1f", v }'; }

printf '%-34s %12s %12s %12s %8s\n' metric "KVM (L0)" "KVM (L1)" "PVM (L1)" "PVM/KVM"
printf '%-34s %12s %12s %12s %8s\n' ---------------------------------- ------------ ------------ ------------ --------

rm -f "$OUT/.compare-missing"
# -a1 -a2, so a metric present on either side gets a row: one that only the
# slow vendor reported is exactly the one worth seeing.
join -j1 -a1 -a2 -o 0,1.2,2.2 -e "" <(metrics "$nested") <(metrics "$pvm") |
while read -r name a b; do
	c=$([ -f "$l0" ] && metrics "$l0" | awk -v n="$name" '$1==n{print $2}')
	ratio=$(awk -v a="$a" -v b="$b" 'BEGIN{ if (a+0==0 || b+0==0) print "-"; else printf "%.2fx", b/a }')
	printf '%-34s %12s %12s %12s %8s\n' "$name" "$(fmt "$c")" "$(fmt "$a")" "$(fmt "$b")" "$ratio"
	[ -n "$a" ] && [ -n "$b" ] || echo "$name" >> "$OUT/.compare-missing"
done

echo
echo "PVM/KVM compares the two L1 columns: same guest, same host kernel," >&2
echo "same nesting depth, different KVM vendor module." >&2

# A row with a blank column is a result that was not collected, and saying
# so is the whole point: a silent blank is indistinguishable from a metric
# nobody thought to measure.
for f in "$nested" "$pvm" "$l0"; do
	[ -f "$f" ] || continue
	t="$(truncated "$f")"
	[ -z "$t" ] || { echo; echo "metric lines a printk cut in half:" >&2; echo "$t" >&2; }
done
if [ -s "$OUT/.compare-missing" ]; then
	echo >&2
	echo "missing from one side, so no comparison was made:" >&2
	sed 's/^/  /' "$OUT/.compare-missing" >&2
	rm -f "$OUT/.compare-missing"
	exit 1
fi
rm -f "$OUT/.compare-missing"
