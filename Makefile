# PVM testbed.
#
# make regress  everything that must pass before anything riskier is run
# make stage0   guest kernel as an ordinary KVM guest -- no PVM host needed
# make stage1   host kernel boots in L1 and kvm-pvm registers
# make stage2   guest kernel under PVM, inside L1
# make full     stage2 with the long suites
# make perf     the measurements, on PVM and on plain KVM, side by side
# make mmu      shadow MMU event counts, both vendors
# make quick    regress + stage2, the shortest thing worth running
# make security negative tests -- on plain KVM and under PVM -- + sanitize
# make sanitize scan every log kept under out/logs for kernel complaints
# make soak     stage2 N times over, then sanitize the lot
# make perf-matrix   perf across 1/2/8/16 vCPUs, both vendors, medians
# make perf-baseline record the last matrix run as this machine's baseline

SHELL := /bin/bash
S     := scripts

KSRC ?= /home/aledbf/Trabajo/github/linux-aledbf
export KSRC

.PHONY: all help deps guest-kernel host-kernel initrd rootfs regress \
        check-rootfs stage0 stage1 stage2 full perf mmu quick sanitize \
        host-sanitize-log security soak perf-matrix perf-baseline \
        clean distclean

# How many times "make soak" repeats stage 2.
SOAK ?= 10

help:
	@sed -n '2,/^$$/p' Makefile | grep '^#' | sed 's/^# \?//'

deps:
	@$(S)/check-deps.sh

guest-kernel:
	@$(S)/build-kernel.sh guest

host-kernel:
	@$(S)/build-kernel.sh host

initrd:
	@$(S)/build-initrd.sh

rootfs: host-kernel
	@$(S)/build-rootfs.sh

# The L1 image takes minutes to bootstrap and may need sudo on machines
# that restrict unprivileged user namespaces, so the stages that need it
# check for it rather than depending on the target and rebuilding it.
check-rootfs:
	@test -f out/images/l1-rootfs.ext4 || { \
		echo "no L1 rootfs -- run 'make rootfs' first" >&2; exit 1; }

# Stage 0 is the one that pays for itself immediately: the guest kernel
# has to boot as a plain KVM guest too, so all of the PIE and early boot
# work can be tested here without a PVM host existing at all.
regress: guest-kernel host-kernel initrd
	@$(S)/regress.sh

stage0: guest-kernel initrd
	@$(S)/run-guest.sh --boot pvh     --suite smoke   --name stage0-pvh-smoke
	@$(S)/run-guest.sh --boot bzimage --suite smoke   --name stage0-bzimage-smoke
	@$(S)/run-guest.sh --boot pvh     --suite default --name stage0-pvh-default

stage1: host-kernel check-rootfs initrd guest-kernel
	@$(S)/run-l1.sh smoke

stage2: host-kernel check-rootfs initrd guest-kernel
	@$(S)/run-l1.sh default

mmu: host-kernel check-rootfs initrd guest-kernel
	@echo "=== shadow MMU events: PVM ==="
	@$(S)/run-l1.sh mmu pvm
	@echo "=== shadow MMU events: ordinary nested KVM ==="
	@$(S)/run-l1.sh mmu intel

full: host-kernel check-rootfs initrd guest-kernel
	@$(S)/run-l1.sh full

perf: guest-kernel initrd host-kernel check-rootfs
	@echo "=== reference: plain KVM on this machine (one layer) ==="
	@$(S)/run-guest.sh --boot pvh --suite perf --name perf-kvm
	@echo "=== baseline: ordinary KVM inside L1 (two layers) ==="
	@$(S)/run-l1.sh perf intel
	@echo "=== PVM inside L1 (two layers) ==="
	@$(S)/run-l1.sh perf pvm
	@$(S)/compare-perf.sh

# The cheapest bug detector here: it needs nobody to have written a test
# for the thing that went wrong.  Every runner already calls it on its own
# log; this scans everything kept from every run so far.
# Negative tests: the things the guest must not be able to do.  Run on
# both, because a case that fails identically under plain KVM is a bug in
# the test, and one that passes there and fails here is a bug in PVM.
security: guest-kernel initrd host-kernel check-rootfs
	@echo "=== negative tests: plain KVM, one layer ==="
	@$(S)/run-guest.sh --boot pvh --suite security --name security-kvm
	@echo "=== negative tests: PVM inside L1 ==="
	@$(S)/run-l1.sh security pvm
	@$(S)/sanitize-log.sh

sanitize:
	@$(S)/sanitize-log.sh

host-sanitize-log: sanitize

quick: regress stage2

# Repetition is what finds the once-in-thirty WARN.  Each iteration keeps
# its own log so the sanitizer at the end has all of them, and a failure
# stops the loop rather than being averaged away.
soak: host-kernel check-rootfs initrd guest-kernel
	@set -e; for i in $$(seq 1 $(SOAK)); do \
		echo "=== soak $$i/$(SOAK) ==="; \
		LOG_SUFFIX=soak$$i $(S)/run-l1.sh default pvm; \
	done
	@$(S)/sanitize-log.sh

# The full default sweep is 40 boots of the perf suite.  Narrow it while
# iterating:  make perf-matrix MATRIX_CPUS="2" MATRIX_REPS=2
perf-matrix: guest-kernel initrd host-kernel check-rootfs
	@$(S)/perf-matrix.sh

# Promote the most recent sweep on this machine to its baseline.
perf-baseline:
	@set -e; \
	f=$$(ls -t out/perf/*.tsv 2>/dev/null | head -1); \
	test -n "$$f" || { echo "no sweep to promote -- run 'make perf-matrix'" >&2; exit 1; }; \
	mkdir -p baselines; \
	cp "$$f" "baselines/$$(basename $$f)"; \
	echo "baseline recorded: baselines/$$(basename $$f)"

clean:
	rm -rf out/logs out/initrd-root out/payload

distclean:
	rm -rf out cache
