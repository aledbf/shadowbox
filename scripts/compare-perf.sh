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
metrics() { sed -n 's/^\(G\[[a-z0-9]*\]: \)\?PVMTEST-METRIC: //p' "$1" | awk '{print $1, $2}' | sort; }

fmt() { awk -v v="$1" 'BEGIN{ if (v=="") print "-"; else printf "%.1f", v }'; }

printf '%-34s %12s %12s %12s %8s\n' metric "KVM (L0)" "KVM (L1)" "PVM (L1)" "PVM/KVM"
printf '%-34s %12s %12s %12s %8s\n' ---------------------------------- ------------ ------------ ------------ --------

join -j1 -a1 <(metrics "$nested") <(metrics "$pvm") | while read -r name a b; do
	c=$([ -f "$l0" ] && metrics "$l0" | awk -v n="$name" '$1==n{print $2}')
	ratio=$(awk -v a="$a" -v b="$b" 'BEGIN{ if (a+0==0) print "-"; else printf "%.2fx", b/a }')
	printf '%-34s %12s %12s %12s %8s\n' "$name" "$(fmt "$c")" "$(fmt "$a")" "$(fmt "$b")" "$ratio"
done

echo
echo "PVM/KVM compares the two L1 columns: same guest, same host kernel," >&2
echo "same nesting depth, different KVM vendor module." >&2
