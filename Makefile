# PVM testbed.
#
# make regress  everything that must pass before anything riskier is run
# make stage0   guest kernel as an ordinary KVM guest -- no PVM host needed
# make stage1   host kernel boots in L1 and kvm-pvm registers
# make stage2   guest kernel under PVM, inside L1
# make full     stage2 with the long suites
# make perf     the measurements, on PVM and on plain KVM, side by side
# make mmu      shadow MMU event counts, both vendors

SHELL := /bin/bash
S     := scripts

KSRC ?= /home/aledbf/Trabajo/github/linux-aledbf
export KSRC

.PHONY: all help deps guest-kernel host-kernel initrd rootfs regress \
        check-rootfs stage0 stage1 stage2 full perf mmu clean distclean

help:
	@sed -n '2,9p' Makefile | sed 's/^# \?//'

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

clean:
	rm -rf out/logs out/initrd-root out/payload

distclean:
	rm -rf out cache
