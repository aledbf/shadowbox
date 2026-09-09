#!/usr/bin/env bash
#
# sanitize-log.sh [-q] [file...]
#
# Scan boot/run logs for the things a kernel says when it has found a
# problem with itself, and fail if any of them are not on the allowlist.
#
# This is the cheapest bug detector in the testbed: it needs nobody to have
# written a test for the thing that went wrong.  A WARN_ON that fires once
# in a fork storm, an RCU stall under the shadow MMU's mmu_lock, a refcount
# underflow on a PVCS page -- none of those make a test case fail, and all
# of them show up here.
#
# Two data files drive it, both greppable and both commented:
#
#   configs/log-fail.txt   what counts as the kernel complaining
#   configs/log-allow.txt  which of those complaints are known and why
#
# Nesting: an L1 log carries the L1 kernel's own output, the guest's output
# behind a "G[machine]:" prefix, and L1's copy of ftrace behind "L1: ".
# The prefixes are stripped before matching so a stall inside the guest is
# caught with the same patterns as one in the host, and the report says
# which layer it came from.

source "$(dirname "$0")/lib.sh"

quiet=0
[ "${1:-}" = "-q" ] && { quiet=1; shift; }

fail_pat="$TESTBED/configs/log-fail.txt"
allow_pat="$TESTBED/configs/log-allow.txt"
[ -f "$fail_pat" ]  || die "missing $fail_pat"
[ -f "$allow_pat" ] || die "missing $allow_pat"

files=("$@")
if [ ${#files[@]} -eq 0 ]; then
	shopt -s nullglob
	files=("$OUT"/logs/*.log)
	shopt -u nullglob
fi
[ ${#files[@]} -gt 0 ] || die "no logs to scan -- run something first"

strip_comments() { grep -vE '^[[:space:]]*(#|$)' "$1"; }

total_hit=0
total_known=0
scanned=0

for f in "${files[@]}"; do
	[ -f "$f" ] || { warn "no such log: $f"; continue; }
	# qemu's own -d output is machine trace, not kernel log.
	case "$(basename "$f")" in *qemu-debug*) continue ;; esac
	scanned=$((scanned + 1))

	# Strip the nesting prefixes, but remember which layer each line came
	# from so the report can say.  The tags are single letters, and not
	# digits, because the report normalises every number away:
	#
	#   G  the PVM guest, running inside L1
	#   T  L1's ftrace relay, which quotes the guest back at us
	#   .  the log's own kernel -- L1 in an L1 log, the guest in a stage 0 one
	layered=$(sed -E 's/^(G\[[a-z0-9_.-]*\]: )/G\t/; t
	                  s/^(L1: )/T\t/; t
	                  s/^/.\t/' "$f")

	hits=$(printf '%s\n' "$layered" |
	       grep -Ef <(strip_comments "$fail_pat") || true)
	[ -z "$hits" ] && continue

	# The allowlist is matched against the line without its layer tag.
	known=$(printf '%s\n' "$hits" |
	        grep -Ef <(strip_comments "$allow_pat") || true)
	unknown=$(printf '%s\n' "$hits" |
	          grep -vEf <(strip_comments "$allow_pat") || true)

	nk=$([ -n "$known" ] && printf '%s\n' "$known" | wc -l || echo 0)
	total_known=$((total_known + nk))

	[ -z "$unknown" ] && {
		[ "$quiet" = 0 ] && [ "$nk" -gt 0 ] &&
			log "$(basename "$f"): $nk known, 0 unknown"
		continue
	}

	nu=$(printf '%s\n' "$unknown" | wc -l)
	total_hit=$((total_hit + nu))

	printf '\n\033[1;31m%s\033[0m  %s unknown, %s known\n' \
		"$(basename "$f")" "$nu" "$nk" >&2
	# Collapse everything that differs run to run -- the printk
	# timestamp, addresses, counters, CPU numbers -- so a stall reported
	# five times reads as one finding rather than five.
	printf '%s\n' "$unknown" |
		sed -E 's/\[[0-9 ]+\.[0-9]+\] //
		        s/0x[0-9a-f]+/0xX/g
		        s/\b[0-9a-f]{8,}\b/HEX/g
		        s/[0-9]+/N/g
		        s/[[:space:]]+/ /g' |
		sort | uniq -c | sort -rn |
		while read -r n line; do
			printf '  %3sx  %s\n' "$n" "$line" >&2
		done
done

if [ "$total_hit" -gt 0 ]; then
	printf '\n\033[1;31msanitize-log: %d unexplained kernel complaints in %d logs\033[0m\n' \
		"$total_hit" "$scanned" >&2
	printf 'If one of them is understood and expected, add it to\n' >&2
	printf '  configs/log-allow.txt  -- with the reason.\n' >&2
	exit 1
fi

[ "$quiet" = 0 ] && printf '\033[32msanitize-log: %d logs clean (%d known lines allowed)\033[0m\n' \
	"$scanned" "$total_known" >&2
exit 0
