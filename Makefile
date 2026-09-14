# PVM testbed.
#
# Every target is a thin call into tools/pvmtest (one static Go binary);
# "pvmtest" with no arguments lists its commands.  How to measure without
# fooling yourself: docs/BRIEFING.md.
#
# The testbed carries no kernel: set KSRC to the tree under test.
#
#   make deps          what is missing on this machine
#   make kernels       guest and host kernels from KSRC (STATS=1: counting host)
#   make initrd        the guest's Go init
#   make rootfs        the L1 root filesystem (once)
#   make regress       guest and host kernels boot under plain KVM here
#   make stage0        guest kernel as an ordinary KVM guest, three ways
#   make stage2        guest under PVM, inside L1, default suite
#   make full          the same with the long suites
#   make check         regress + batteries/check.pvm
#   make perf          one perf run, PVM next to kvm-intel
#   make perf-matrix   batteries/perf-matrix.pvm against the baseline
#   make perf-baseline record the last matrix as this machine's baseline
#   make mmu           shadow MMU event counts, both vendors
#   make exits CASE=   exits attributed to one case
#   make failclosed    the hosts kvm-pvm must refuse
#   make soak          the default suite ten times
#   make sanitize      scan every log under out/logs
#   make battery B=<name> [J=<jobs>]   any batteries/<name>.pvm

SHELL   := /bin/bash
PVMTEST := out/bin/pvmtest
J       ?= 1

export KSRC

.PHONY: all help deps pvmtest kernels guest-kernel host-kernel initrd agent \
        hosttests selftests rootfs regress stage0 stage1 stage2 full check \
        perf perf-matrix perf-baseline mmu exits failclosed soak sanitize \
        battery clean distclean

help:
	@sed -n '2,/^$$/p' Makefile | grep '^#' | sed 's/^# \?//'

pvmtest:
	@cd tools/pvmtest && CGO_ENABLED=0 go build -trimpath -o ../../$(PVMTEST) .

deps: pvmtest
	@$(PVMTEST) deps

kernels: guest-kernel host-kernel

guest-kernel: pvmtest
	@$(PVMTEST) build guest

host-kernel: pvmtest
	@$(PVMTEST) build host $(if $(STATS),-stats)

initrd: pvmtest
	@$(PVMTEST) build initrd

agent: pvmtest
	@$(PVMTEST) build agent

hosttests: pvmtest
	@$(PVMTEST) build hosttests

selftests: pvmtest
	@$(PVMTEST) build selftests

rootfs: pvmtest agent
	@$(PVMTEST) build rootfs

regress: pvmtest
	@$(PVMTEST) regress

stage0: pvmtest
	@$(PVMTEST) boot guest -boot pvh     -suite smoke   -name stage0-pvh-smoke
	@$(PVMTEST) boot guest -boot bzimage -suite smoke   -name stage0-bzimage-smoke
	@$(PVMTEST) boot guest -boot pvh     -suite default -name stage0-pvh-default

stage1: pvmtest
	@$(PVMTEST) boot l1 -suite smoke

stage2: pvmtest
	@$(PVMTEST) boot l1 -suite default

full: pvmtest
	@$(PVMTEST) boot l1 -suite full

check: regress
	@$(PVMTEST) run -j $(or $(filter-out 1,$(J)),2) batteries/check.pvm

perf: pvmtest
	@$(PVMTEST) run batteries/perf-quick.pvm

perf-matrix: pvmtest
	@$(PVMTEST) run batteries/perf-matrix.pvm

# Promote the most recent matrix on this machine to its baseline.
perf-baseline:
	@set -e; f=$$(ls -t out/perf/*.tsv 2>/dev/null | head -1); \
	test -n "$$f" || { echo "no matrix to promote -- run 'make perf-matrix'" >&2; exit 1; }; \
	mkdir -p baselines; cp "$$f" "baselines/$$(basename $$f)"; \
	echo "baseline recorded: baselines/$$(basename $$f)"

mmu: pvmtest
	@$(PVMTEST) boot l1 -suite mmu -vendor pvm
	@$(PVMTEST) boot l1 -suite mmu -vendor intel

exits: pvmtest
	@$(PVMTEST) exits "$(or $(CASE),perf/context-switch)"

failclosed: pvmtest
	@$(PVMTEST) run -only refuse-kpti,refuse-fsgsbase,refuse-pcid batteries/check.pvm

soak: pvmtest
	@$(PVMTEST) run batteries/soak.pvm

sanitize: pvmtest
	@$(PVMTEST) sanitize

battery: pvmtest
	@test -n "$(B)" || { echo "make battery B=<name>; see batteries/" >&2; exit 1; }
	@$(PVMTEST) run -j $(J) batteries/$(B).pvm

clean:
	rm -rf out/logs out/results

distclean:
	rm -rf out cache
