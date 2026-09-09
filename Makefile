# PVM testbed.
#
# make regress  everything that must pass before anything riskier is run
# make stage0   guest kernel as an ordinary KVM guest -- no PVM host needed
# make stage1   host kernel boots in L1 and kvm-pvm registers
# make stage2   guest kernel under PVM, inside L1
# make full     stage2 with the long suites
# make perf     the measurements, on PVM and on plain KVM, side by side

SHELL := /bin/bash
S     := scripts

KSRC ?= /home/aledbf/Trabajo/github/linux-aledbf
export KSRC

.PHONY: all help deps guest-kernel host-kernel initrd rootfs regress \
        stage0 stage1 stage2 full perf clean distclean

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

# Stage 0 is the one that pays for itself immediately: the guest kernel
# has to boot as a plain KVM guest too, so all of the PIE and early boot
# work can be tested here without a PVM host existing at all.
regress: guest-kernel host-kernel initrd
	@$(S)/regress.sh

stage0: guest-kernel initrd
	@$(S)/run-guest.sh --boot pvh     --suite smoke   --name stage0-pvh-smoke
	@$(S)/run-guest.sh --boot bzimage --suite smoke   --name stage0-bzimage-smoke
	@$(S)/run-guest.sh --boot pvh     --suite default --name stage0-pvh-default

stage1: host-kernel rootfs initrd guest-kernel
	@$(S)/run-l1.sh smoke

stage2: host-kernel rootfs initrd guest-kernel
	@$(S)/run-l1.sh default

full: host-kernel rootfs initrd guest-kernel
	@$(S)/run-l1.sh full

perf: guest-kernel initrd host-kernel rootfs
	@echo "=== baseline: plain KVM ==="
	@$(S)/run-guest.sh --boot pvh --suite perf --name perf-kvm
	@echo "=== under PVM ==="
	@$(S)/run-l1.sh perf
	@$(S)/compare-perf.sh

clean:
	rm -rf out/logs out/initrd-root out/payload

distclean:
	rm -rf out cache
