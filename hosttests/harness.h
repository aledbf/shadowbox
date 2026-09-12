/* SPDX-License-Identifier: GPL-2.0 */
/*
 * Shared scaffolding for the host-side PVM tests: TAP-ish result lines, a
 * vCPU with guest memory behind it, and the ioctl wrappers the cases use.
 *
 * Header-only and static, because scripts/build-hosttests.sh builds every
 * every hosttest on its own, with nothing but libc and the tree's own uapi
 * headers.  Not the KVM selftests library: it builds a guest that expects
 * to run at CPL0 with its own page tables, which is exactly what a PVM VM
 * does not give it.
 */
#ifndef PVM_HOSTTEST_HARNESS_H
#define PVM_HOSTTEST_HARNESS_H

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <unistd.h>

#include <linux/kvm.h>

#ifndef X86_CR4_PKE
#define X86_CR4_PKE (1UL << 22)
#endif

/* From arch/x86/include/uapi/asm/pvm_para.h. */
#define MSR_PVM_VCPU_STRUCT		0x4b564df1
#define MSR_PVM_EVENT_ENTRY		0x4b564df4
#define MSR_PVM_RETU_RIP		0x4b564df5
#define PVM_VIRTUAL_MSR_BASE		0x4b564df0
#define PVM_VIRTUAL_MSR_MAX_NR		15

#define GUEST_PHYS_BASE	0x100000UL
#define GUEST_MEM_SIZE	(8UL << 20)
#define PVCS_GPA	(GUEST_PHYS_BASE + 0x2000)

static int pass, fail;
static const char *current_case;

static inline void ok(const char *fmt, ...)
{
	va_list ap;
	va_start(ap, fmt);
	printf("ok %d - %s (", ++pass + fail, current_case);
	vprintf(fmt, ap);
	printf(")\n");
	va_end(ap);
	fflush(stdout);
}

static inline void nok(const char *fmt, ...)
{
	va_list ap;
	va_start(ap, fmt);
	printf("not ok %d - %s: ", pass + ++fail, current_case);
	vprintf(fmt, ap);
	printf("\n");
	va_end(ap);
	fflush(stdout);
}

static inline void die(const char *what)
{
	fprintf(stderr, "bail out! %s: %s\n", what, strerror(errno));
	printf("Bail out! %s: %s\n", what, strerror(errno));
	exit(2);
}

static inline const char *exit_reason_name(int r)
{
	switch (r) {
	case KVM_EXIT_UNKNOWN:		return "UNKNOWN";
	case KVM_EXIT_EXCEPTION:	return "EXCEPTION";
	case KVM_EXIT_IO:		return "IO";
	case KVM_EXIT_HLT:		return "HLT";
	case KVM_EXIT_MMIO:		return "MMIO";
	case KVM_EXIT_SHUTDOWN:		return "SHUTDOWN";
	case KVM_EXIT_FAIL_ENTRY:	return "FAIL_ENTRY";
	case KVM_EXIT_INTR:		return "INTR";
	case KVM_EXIT_INTERNAL_ERROR:	return "INTERNAL_ERROR";
	default:			return "?";
	}
}

struct vm {
	int kvm, vm, vcpu;
	struct kvm_run *run;
	size_t run_size;
	void *mem;
	struct kvm_cpuid2 *cpuid;   /* what KVM_GET_SUPPORTED_CPUID said */
};

#define MAX_CPUID_ENTRIES 256

/* The CPUID KVM says it supports, which for a PVM host is what PVM chose
 * to advertise in pvm_set_cpu_caps().  Also what a VMM hands the vCPU, and
 * without it the vCPU has no guest_cpu_cap at all -- CR4.PKE then reads as
 * a reserved bit and KVM_SET_SREGS refuses it.
 */
static inline struct kvm_cpuid2 *get_supported_cpuid(struct vm *v)
{
	struct kvm_cpuid2 *c;
	size_t bytes = sizeof(*c) + MAX_CPUID_ENTRIES * sizeof(c->entries[0]);

	c = calloc(1, bytes);
	if (!c)
		die("calloc cpuid");
	c->nent = MAX_CPUID_ENTRIES;
	if (ioctl(v->kvm, KVM_GET_SUPPORTED_CPUID, c) < 0)
		die("KVM_GET_SUPPORTED_CPUID");
	return c;
}

static inline bool cpuid_has(struct kvm_cpuid2 *c, uint32_t function, uint32_t index,
		      int reg, unsigned bit)
{
	uint32_t i;

	for (i = 0; i < c->nent; i++) {
		struct kvm_cpuid_entry2 *e = &c->entries[i];
		uint32_t regs[4];

		if (e->function != function)
			continue;
		if ((e->flags & KVM_CPUID_FLAG_SIGNIFCANT_INDEX) && e->index != index)
			continue;
		regs[0] = e->eax; regs[1] = e->ebx; regs[2] = e->ecx; regs[3] = e->edx;
		return regs[reg] & (1u << bit);
	}
	return false;
}

/*
 * KVM_SET_MSRS returns how many entries it managed to write, so a return
 * of 0 for a single-entry list is the vendor's set_msr() having said no.
 * A negative return is the ioctl itself failing, which is a different
 * thing and worth distinguishing in the message.
 */
static inline int set_msr(struct vm *v, uint32_t index, uint64_t data)
{
	struct {
		struct kvm_msrs hdr;
		struct kvm_msr_entry entry;
	} m = { .hdr = { .nmsrs = 1 }, .entry = { .index = index, .data = data } };
	int r = ioctl(v->vcpu, KVM_SET_MSRS, &m);

	return r < 0 ? -errno : r;
}

static inline int get_msr(struct vm *v, uint32_t index, uint64_t *data)
{
	struct {
		struct kvm_msrs hdr;
		struct kvm_msr_entry entry;
	} m = { .hdr = { .nmsrs = 1 }, .entry = { .index = index } };
	int r = ioctl(v->vcpu, KVM_GET_MSRS, &m);

	if (r < 0)
		return -errno;
	if (r != 1)
		return 0;
	*data = m.entry.data;
	return 1;
}

static inline int set_memslot(struct vm *v, uint32_t slot, uint64_t gpa,
		       uint64_t size, void *hva, uint32_t flags)
{
	struct kvm_userspace_memory_region r = {
		.slot = slot,
		.flags = flags,
		.guest_phys_addr = gpa,
		.memory_size = size,
		.userspace_addr = (uint64_t)hva,
	};

	return ioctl(v->vm, KVM_SET_USER_MEMORY_REGION, &r) < 0 ? -errno : 0;
}

static inline void vm_setup(struct vm *v)
{
	v->kvm = open("/dev/kvm", O_RDWR | O_CLOEXEC);
	if (v->kvm < 0)
		die("open /dev/kvm");

	v->vm = ioctl(v->kvm, KVM_CREATE_VM, 0);
	if (v->vm < 0)
		die("KVM_CREATE_VM");

	v->mem = mmap(NULL, GUEST_MEM_SIZE, PROT_READ | PROT_WRITE,
		      MAP_PRIVATE | MAP_ANONYMOUS | MAP_NORESERVE, -1, 0);
	if (v->mem == MAP_FAILED)
		die("mmap guest memory");

	if (set_memslot(v, 0, GUEST_PHYS_BASE, GUEST_MEM_SIZE, v->mem, 0))
		die("KVM_SET_USER_MEMORY_REGION");

	v->vcpu = ioctl(v->vm, KVM_CREATE_VCPU, 0);
	if (v->vcpu < 0)
		die("KVM_CREATE_VCPU");

	v->cpuid = get_supported_cpuid(v);
	if (ioctl(v->vcpu, KVM_SET_CPUID2, v->cpuid) < 0)
		die("KVM_SET_CPUID2");

	v->run_size = ioctl(v->kvm, KVM_GET_VCPU_MMAP_SIZE, 0);
	v->run = mmap(NULL, v->run_size, PROT_READ | PROT_WRITE, MAP_SHARED,
		      v->vcpu, 0);
	if (v->run == MAP_FAILED)
		die("mmap kvm_run");
}

static inline void vm_teardown(struct vm *v)
{
	munmap(v->run, v->run_size);
	close(v->vcpu);
	close(v->vm);
	munmap(v->mem, GUEST_MEM_SIZE);
	free(v->cpuid);
	close(v->kvm);
}

/*
 * Every case in these programs is a negative test whose real assertion is
 * "the host is still alive afterwards", so running them against the wrong
 * vendor module proves nothing at all.
 */
static inline int is_pvm_host(void)
{
	char buf[4096];
	int fd, n;

	fd = open("/proc/modules", O_RDONLY);
	if (fd < 0)
		return 0;
	n = read(fd, buf, sizeof(buf) - 1);
	close(fd);
	if (n <= 0)
		return 0;
	buf[n] = 0;
	return strstr(buf, "kvm_pvm") != NULL;
}

#endif /* PVM_HOSTTEST_HARNESS_H */
