#!/usr/bin/env bash
#
# case-stats.sh <l1 log> [counter-regex]
#
# Per-case counter deltas from a run with stats=on on a CONFIG_KVM_PVM_STATS
# host: now "pvmtest stats", kept under this name for the logs and notes
# that cite it.
source "$(dirname "$0")/lib.sh"
[ -x "$OUT/bin/pvmtest" ] || make -C "$TESTBED" -s pvmtest
exec "$OUT/bin/pvmtest" stats -filter "${2:-.}" "${1:?usage: case-stats.sh <log> [counter-regex]}"
