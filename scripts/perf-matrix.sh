#!/usr/bin/env bash
#
# perf-matrix.sh
#
# The perf suite, run across a range of vCPU counts, on both KVM vendors,
# repeated, reduced to medians, and compared against a saved baseline.
#
# Why each of those:
#
#   across vCPU counts   the single-threaded numbers said fork/exec had no
#                        scaling problem; the parallel ones said 3.82x at
#                        2 CPUs and 6.78x at 8.  One point on that curve is
#                        not a measurement.
#   both vendors         PVM against ordinary nested KVM, same guest, same
#                        machine, same nesting depth.  Against bare-metal
#                        KVM it would be measuring the nesting.
#   repeated + median    a single run of a benchmark inside two layers of
#                        virtualisation is noise with a number attached.
#   against a baseline   absolute thresholds would fail on a slower laptop
#                        and pass on a faster one.  What is worth failing
#                        on is this machine getting worse than it was.
#
# Every metric here is lower-is-better (ns, us), which is what makes the
# regression test a single comparison rather than a per-metric direction.
#
#   MATRIX_CPUS       vCPU counts to sweep      (default "1 2 8 16")
#   MATRIX_REPS       runs per point            (default 5)
#   MATRIX_VENDORS    KVM vendors               (default "pvm intel")
#   MATRIX_THRESHOLD  percent worse than the baseline that fails (default 15)
#   MATRIX_REUSE_LOGS set: boot nothing, reduce the l1-perf-*-cpuN-rM logs
#                     already in out/logs (after a sweep whose report died)
#
# A full default sweep is 4 x 2 x 5 = 40 boots of the perf suite.  Narrow
# MATRIX_CPUS while iterating.

source "$(dirname "$0")/lib.sh"

CPUS="${MATRIX_CPUS:-1 2 8 16}"
REPS="${MATRIX_REPS:-5}"
VENDORS="${MATRIX_VENDORS:-pvm intel}"
THRESHOLD="${MATRIX_THRESHOLD:-15}"

# What identifies a baseline is the machine and the kernel it is hosting
# on -- not the kernel under test, which is the thing being measured.
cpu_model=$(sed -n 's/^model name[[:space:]]*: //p' /proc/cpuinfo | head -1 |
            tr -cs 'A-Za-z0-9' '-' | sed 's/-*$//' | cut -c1-40)
env_id="$(uname -r)__${cpu_model}"
results="$OUT/perf/$env_id.tsv"
baseline="$TESTBED/baselines/$env_id.tsv"

mkdir -p "$OUT/perf" "$OUT/logs"

# MATRIX_KERNEL_ID: what the logs being reused were built from, which need
# not be what the tree says now.
kernel_id=${MATRIX_KERNEL_ID:-$(cd "$KSRC" && git describe --always --dirty 2>/dev/null || echo unknown)}

# A counting build puts a memory increment on every fast path it counts, so
# its timings are not the kernel's.  It is for "make exits", not for this.
if grep -qx 'CONFIG_KVM_PVM_STATS=y' "$OUT/build-host/.config" 2>/dev/null &&
   [ -z "${ALLOW_STATS_BUILD:-}" ]; then
	die "the host kernel in out/ was built with CONFIG_KVM_PVM_STATS; rebuild without HOST_CONFIG_EXTRA (or set ALLOW_STATS_BUILD=1)"
fi

log "environment: $env_id"
log "kernel under test: $kernel_id"
log "sweep: cpus=[$CPUS] vendors=[$VENDORS] reps=$REPS"

# --- collect -------------------------------------------------------------

raw="$OUT/perf/.raw.$$"
: > "$raw"
trap 'rm -f "$raw"' EXIT

#
# Reps outermost, vendors innermost, on purpose.  Running every rep of one
# vendor and then every rep of the other lets slow drift -- the machine
# warming up over an hour of sweeping -- land entirely on one side of the
# comparison.  Interleaving makes both vendors see the same drift.  This
# matters: two sweeps of the *same* build, an hour apart, have differed by
# nearly 50% on page-fault.
for n in $CPUS; do
	for rep in $(seq 1 "$REPS"); do
		for vendor in $VENDORS; do
			tag="cpu$n-r$rep"
			log "cpus=$n vendor=$vendor rep=$rep/$REPS"
			l="$OUT/logs/l1-perf-$vendor-$tag.log"
			if [ -n "${MATRIX_REUSE_LOGS:-}" ]; then
				[ -f "$l" ] || { warn "no log $l"; continue; }
			elif ! GUEST_CPUS="$n" LOG_SUFFIX="$tag" \
			     "$TESTBED/scripts/run-l1.sh" perf "$vendor" \
			     > "$OUT/logs/matrix-$vendor-$tag.out" 2>&1; then
				# A run "fails" most often because L1's console
				# printed into the middle of the guest's result
				# line, which is then not recognised.  The metric
				# lines carry their own terminator for exactly that
				# reason, so what decides whether this run counts is
				# whether any case failed and whether its metrics
				# arrived whole -- not the result line.
				if grep -aq 'not ok' "$l" 2>/dev/null ||
				   ! grep -aq 'PVMTEST-METRIC: .*#END' "$l" 2>/dev/null; then
					warn "cpus=$n vendor=$vendor rep=$rep failed; see out/logs/matrix-$vendor-$tag.out"
					continue
				fi
				warn "cpus=$n vendor=$vendor rep=$rep: no clean result line, but no case failed; keeping its metrics"
			fi
			# The guest's metric lines carry the agent's machine tag
			# when they came out of L1.  They also arrive over a
			# serial console, so every line ends in CR -- which ends
			# up glued to the unit field and makes it compare equal
			# to nothing.
			sed -n 's/^\(G\[[a-z0-9]*\]: \)\?PVMTEST-METRIC: //p' \
				"$l" | tr -d '\r' |
				awk -v n="$n" -v v="$vendor" '$NF == "#END" {print $1"\t"n"\t"v"\t"$2"\t"$3}' \
				>> "$raw"
		done
	done
done

[ -s "$raw" ] || die "no metrics collected -- every run failed"

# --- reduce to medians ---------------------------------------------------

{
	printf '# pvm-testbed perf matrix\n'
	printf '# env\t%s\n' "$env_id"
	printf '# kernel\t%s\n' "$kernel_id"
	printf '# date\t%s\n' "$(date -Is)"
	printf '# reps\t%s\n' "$REPS"
	printf 'metric\tcpus\tvendor\tmedian\tmin\tmax\tn\tunit\n'
	sort "$raw" | awk -F'\t' '
	{ key = $1 "\t" $2 "\t" $3; vals[key] = vals[key] " " $4; unit[key] = $5 }
	END {
		for (k in vals) {
			n = split(vals[k], a, " ")
			# split leaves a[1] empty from the leading space
			m = 0
			for (i = 1; i <= n; i++) if (a[i] != "") b[++m] = a[i] + 0
			for (i = 2; i <= m; i++)
				for (j = i; j > 1 && b[j] < b[j-1]; j--) { t = b[j]; b[j] = b[j-1]; b[j-1] = t }
			med = (m % 2) ? b[(m+1)/2] : (b[m/2] + b[m/2+1]) / 2
			printf "%s\t%.4g\t%.4g\t%.4g\t%d\t%s\n", k, med, b[1], b[m], m, unit[k]
			delete b
		}
	}' | sort -t"$(printf '\t')" -k1,1 -k2,2n -k3,3
} > "$results"

log "wrote $results"

# --- report --------------------------------------------------------------

# Column 4 is the median, 5 the minimum, 6 the maximum.
field() { awk -F'\t' -v m="$1" -v c="$2" -v v="$3" -v f="$5" \
	'$1==m && $2==c && $3==v {print $f}' "$4"; }
med() { field "$1" "$2" "$3" "$4" 4; }

fail=0
have_baseline=0
[ -f "$baseline" ] && have_baseline=1 || \
	warn "no baseline at baselines/$env_id.tsv -- run 'make perf-baseline' to record this run as one"

printf '\n%-42s %5s %12s %7s %12s %8s %s\n' \
	metric cpus PVM 'spread' KVM 'PVM/KVM' 'vs baseline'
printf '%-42s %5s %12s %7s %12s %8s %s\n' \
	'------------------------------------------' ----- ------------ ------- ------------ -------- -----------

# Only the timings.  A metric in "count" units -- parallel-fork reporting
# how many CPUs it actually got -- is a property of the run, not a result,
# and "lower is better" is meaningless for it.
metrics=$(awk -F'\t' '$1 !~ /^#/ && $1!="metric" && ($8=="ns"||$8=="us"||$8=="s") {print $1}' \
	"$results" | sort -u)
for m in $metrics; do
	for n in $CPUS; do
		p=$(med "$m" "$n" pvm "$results")
		k=$(med "$m" "$n" intel "$results")
		[ -z "$p" ] && continue
		ratio=$(awk -v a="$k" -v b="$p" 'BEGIN{ if (a+0==0) print "-"; else printf "%.2fx", b/a }')

		# How far apart the runs of this very point were.  A delta
		# smaller than this says nothing: it was measured twice on the
		# same build and moved by more than that.
		pmin=$(field "$m" "$n" pvm "$results" 5)
		pmax=$(field "$m" "$n" pvm "$results" 6)
		spread=$(awk -v lo="$pmin" -v hi="$pmax" -v md="$p" \
			'BEGIN{ if (md+0==0) print "-"; else printf "%.0f%%", (hi-lo)/md*100 }')

		delta="-"
		if [ "$have_baseline" = 1 ]; then
			bp=$(med "$m" "$n" pvm "$baseline")
			bmax=$(field "$m" "$n" pvm "$baseline" 6)
			if [ -n "$bp" ]; then
				delta=$(awk -v now="$p" -v was="$bp" \
					'BEGIN{ if (was+0==0) print "-"; else printf "%+.1f%%", (now-was)/was*100 }')
				# Lower is better for every metric here, so a
				# positive delta is a regression -- but only if
				# it is bigger than the threshold *and* lands
				# outside the range the baseline's own runs
				# covered.  Without the second test the matrix
				# reports run-to-run noise as a regression:
				# these benchmarks boot a fresh L1 per run, and
				# two sweeps of the same build have been seen
				# 30% apart on page-fault.
				worse=$(awk -v now="$p" -v was="$bp" -v t="$THRESHOLD" \
					-v hi="$bmax" \
					'BEGIN{ print (was+0>0 &&
						       (now-was)/was*100 > t &&
						       now+0 > hi+0) ? 1 : 0 }')
				if [ "$worse" = 1 ]; then
					delta="$delta  REGRESSION"
					fail=1
				elif awk -v now="$p" -v was="$bp" -v t="$THRESHOLD" \
					'BEGIN{ exit !(was+0>0 && (now-was)/was*100 > t) }'; then
					delta="$delta  (inside baseline spread)"
				fi
			fi
		fi
		printf '%-42s %5s %12.1f %7s %12s %8s %s\n' \
			"$m" "$n" "$p" "$spread" \
			"$(awk -v v="$k" 'BEGIN{ if (v=="") print "-"; else printf "%.1f", v }')" \
			"$ratio" "$delta"
	done
done

echo
echo "PVM and KVM are the two L1 columns: same guest, same host kernel, same" >&2
echo "nesting depth, different vendor module.  Medians of $REPS runs." >&2

if [ "$fail" = 1 ]; then
	printf '\n\033[1;31mperf-matrix: at least one metric is more than %s%% worse than the baseline\033[0m\n' \
		"$THRESHOLD" >&2
	exit 1
fi
exit 0
