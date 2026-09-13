#!/usr/bin/env bash
#
# pf-classify.sh [log-tag] [cpus]
#
# Per-page shadow fault classification and the reflected-fault time share,
# from the marked counting runs of docs/TUNING-LOG-v3.md:
#
#   GUEST_CPUS=$n GUEST_APPEND=pvmtest.statsmsr=0x4b564d2f,pvmtest.only=fault \
#       LOG_SUFFIX=<tag>-fault-cpu$n scripts/run-l1.sh perf pvm
#   (and pvmtest.only=perf/fault-scaling, LOG_SUFFIX=<tag>-perf-fault-scaling-cpu$n)
#
# on a CONFIG_KVM_PVM_STATS host carrying the reflect/exit-class counters.
# Host time is from the vCPU's exit to its next entry, summed over vCPUs; the
# share is of wall time per page times vCPUs, so it is a lower bound on what
# removing the exit could save and ignores the hardware and switcher cost.
# Page counts are the cases' own constants.

cd "$(dirname "$0")/.."
row() { # log column-name pages metric n
  local log=$1 col=$2 pages=$3 metric=$4 n=$5
  scripts/case-stats.sh "$log" '^(pf_exit_umod|pf_exit_smod|reflect_np|pf_fixed|exit_ns_pf_reflect_np|exit_n_pf_reflect_np|exit_ns_pf_fixed|exit_n_pf_fixed|mmu_lock_wait_ns)$' |
  awk -v col="$col" -v pages="$pages" -v n="$n" -v w="$(grep -a "PVMTEST-METRIC: $metric " "$log" | head -1 | awk '{print $(NF-2)}')" '
    NR==1 { for (i=2;i<=NF;i++) if ($i==col) c=i; next }
    { v[$1]=$c }
    END {
      ex=(v["pf_exit_umod"]+v["pf_exit_smod"])/pages
      rn=v["exit_ns_pf_reflect_np"]/v["exit_n_pf_reflect_np"]
      fx=v["exit_ns_pf_fixed"]/v["exit_n_pf_fixed"]
      vt=w*n
      printf "%-16s n=%d exits/pg=%.3f reflNP/pg=%.3f fixed/pg=%.3f host_ns/refl=%.0f host_ns/fixed=%.0f lockwait/pg=%.0f wall/pg=%.0f vcpu/pg=%.0f refl_share=%.0f%% fixed_share=%.0f%%\n",
        col, n, ex, v["reflect_np"]/pages, v["pf_fixed"]/pages, rn, fx, v["mmu_lock_wait_ns"]/pages, w, vt,
        100*v["exit_ns_pf_reflect_np"]/pages/vt, 100*v["exit_ns_pf_fixed"]/pages/vt
    }'
}
p=${1:-v3count}
for n in ${2:-1 2 4 8}; do
  row out/logs/l1-perf-pvm-$p-fault-cpu$n.log page-fault 65536 perf/page-fault.ns_per_fault 1
  row out/logs/l1-perf-pvm-$p-fault-cpu$n.log parallel-fault $((n*6*4096)) perf/parallel-fault.ns_per_page_cycle $n
  [ -f out/logs/l1-perf-pvm-$p-perf-fault-scaling-cpu$n.log ] && row out/logs/l1-perf-pvm-$p-perf-fault-scaling-cpu$n.log fault-scaling $((n*32768)) perf/fault-scaling.ns_per_page $n
done
