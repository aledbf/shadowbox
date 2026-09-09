#!/usr/bin/env bash
#
# Puts the PVM numbers next to the plain-KVM ones.  Reads the metric
# lines the guest printed; no thresholds, because the only useful
# comparison is against the same guest on the same machine.

source "$(dirname "$0")/lib.sh"

kvm="$OUT/logs/perf-kvm.log"
pvm="$OUT/logs/l1-perf.log"
[ -f "$kvm" ] || die "no baseline log: run 'make perf'"
[ -f "$pvm" ] || die "no PVM log: run 'make perf'"

metrics() { sed -n 's/^\(G: \)\?PVMTEST-METRIC: //p' "$1"; }

printf '%-32s %14s %14s %8s\n' metric kvm pvm ratio
printf '%-32s %14s %14s %8s\n' -------------------------------- -------------- -------------- --------
join -j1 \
	<(metrics "$kvm" | awk '{print $1, $2}' | sort) \
	<(metrics "$pvm" | awk '{print $1, $2}' | sort) \
| while read -r name a b; do
	ratio=$(awk -v a="$a" -v b="$b" 'BEGIN{ if (a+0==0) print "-"; else printf "%.2fx", b/a }')
	printf '%-32s %14s %14s %8s\n' "$name" "$a" "$b" "$ratio"
done
