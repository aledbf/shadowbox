// SPDX-License-Identifier: GPL-2.0
/*
 * Host-side tests for the PVM ABI.
 *
 * Everything the guest-side suite can reach runs at guest CPL3.  The PVM
 * MSRs, the PVCS pinning and the memslot lifetime around it are reachable
 * only from the VMM, through the KVM API, and nothing tested them.  This
 * is the program that does.
 *
 * The contract being checked is in arch/x86/kvm/pvm/pvm.c:
 *
 *   MSR_PVM_LINEAR_ADDRESS_RANGE  pvm_check_and_set_msr_linear_address_range()
 *                                 rejects anything that is not a well formed,
 *                                 in-bounds range; 0 resets to the default.
 *   MSR_PVM_VCPU_STRUCT           must be page aligned.  An address with no
 *                                 memslot behind it is *accepted* on purpose --
 *                                 a VMM restoring MSRs before memory regions
 *                                 would otherwise fail -- and turned into a
 *                                 KVM_REQ_GPC_REFRESH that triple faults the
 *                                 guest at entry if it is still bad.  So the
 *                                 assertion is that the guest dies and the
 *                                 host does not.
 *   MSR_PVM_EVENT_ENTRY           must be canonical, and so must +256 and +512.
 *
 * Every case here is a negative test whose real assertion is "the host is
 * still alive afterwards".  A crashed host does not print "not ok"; it
 * prints nothing, which is why the harness outside also checks the kernel
 * log.  Run it under scripts/sanitize-log.sh, not on its own.
 *
 * Built against the tree's own uapi headers, with nothing but libc: the
 * KVM selftests library would pull in a guest ABI that a PVM VM does not
 * have.
 */

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdarg.h>
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
#define MSR_PVM_LINEAR_ADDRESS_RANGE	0x4b564df0
#define MSR_PVM_VCPU_STRUCT		0x4b564df1
#define MSR_PVM_EVENT_ENTRY		0x4b564df4
#define MSR_PVM_RETU_RIP		0x4b564df5
#define PVM_VIRTUAL_MSR_BASE		0x4b564df0
#define PVM_VIRTUAL_MSR_MAX_NR		15

#define GUEST_PHYS_BASE	0x100000UL
#define GUEST_MEM_SIZE	(8UL << 20)
#define PVCS_GPA	(GUEST_PHYS_BASE + 0x2000)

static const char *exit_reason_name(int r);

static int pass, fail;
static const char *current_case;

static void ok(const char *fmt, ...)
{
	va_list ap;
	va_start(ap, fmt);
	printf("ok %d - %s (", ++pass + fail, current_case);
	vprintf(fmt, ap);
	printf(")\n");
	va_end(ap);
	fflush(stdout);
}

static void nok(const char *fmt, ...)
{
	va_list ap;
	va_start(ap, fmt);
	printf("not ok %d - %s: ", pass + ++fail, current_case);
	vprintf(fmt, ap);
	printf("\n");
	va_end(ap);
	fflush(stdout);
}

static void die(const char *what)
{
	fprintf(stderr, "bail out! %s: %s\n", what, strerror(errno));
	printf("Bail out! %s: %s\n", what, strerror(errno));
	exit(2);
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
static struct kvm_cpuid2 *get_supported_cpuid(struct vm *v)
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

static bool cpuid_has(struct kvm_cpuid2 *c, uint32_t function, uint32_t index,
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
static int set_msr(struct vm *v, uint32_t index, uint64_t data)
{
	struct {
		struct kvm_msrs hdr;
		struct kvm_msr_entry entry;
	} m = { .hdr = { .nmsrs = 1 }, .entry = { .index = index, .data = data } };
	int r = ioctl(v->vcpu, KVM_SET_MSRS, &m);

	return r < 0 ? -errno : r;
}

static int get_msr(struct vm *v, uint32_t index, uint64_t *data)
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

static int set_memslot(struct vm *v, uint32_t slot, uint64_t gpa,
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

static void vm_setup(struct vm *v)
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

static void vm_teardown(struct vm *v)
{
	munmap(v->run, v->run_size);
	close(v->vcpu);
	close(v->vm);
	munmap(v->mem, GUEST_MEM_SIZE);
	free(v->cpuid);
	close(v->kvm);
}

/* --- MSR_PVM_LINEAR_ADDRESS_RANGE ------------------------------------- */

static void test_linear_range(struct vm *v)
{
	/*
	 * The encoding is four 9-bit PML indices at bits 0/16/32/48, with
	 * every byte above each index required to be 0xff.  These are the
	 * ways of getting that wrong.
	 */
	static const struct {
		const char *why;
		uint64_t val;
	} bad[] = {
		{ "top bytes not all set",	0x0000000000000000ULL | 0x1 },
		{ "one top byte clear",		0x00ff00ff00ff0100ULL },
		{ "pml4 start > end",		0xff00ff00ff00ff00ULL | (0x100ULL << 0) | (0x0ULL << 16) },
		{ "zero-size pml4 range not 0x1ff",
						0xff00ff00ff00ff00ULL | (0x10ULL << 0) | (0x10ULL << 16) },
		{ "all bits set",		~0ULL },
	};
	uint64_t saved = 0, back = 0;
	size_t i;

	current_case = "pvm/linear-range/roundtrip";
	if (get_msr(v, MSR_PVM_LINEAR_ADDRESS_RANGE, &saved) != 1) {
		nok("MSR_PVM_LINEAR_ADDRESS_RANGE is not readable");
		return;
	}
	if (saved == 0)
		nok("the default range reads back as 0");
	else
		ok("default = %#llx", (unsigned long long)saved);

	for (i = 0; i < sizeof(bad) / sizeof(bad[0]); i++) {
		char name[128];

		snprintf(name, sizeof(name), "pvm/linear-range/reject-%zu", i);
		current_case = name;
		if (set_msr(v, MSR_PVM_LINEAR_ADDRESS_RANGE, bad[i].val) > 0)
			nok("%#llx (%s) was accepted",
			    (unsigned long long)bad[i].val, bad[i].why);
		else
			ok("%s rejected", bad[i].why);
	}

	/* 0 is the documented way to ask for the default back. */
	current_case = "pvm/linear-range/zero-resets";
	if (set_msr(v, MSR_PVM_LINEAR_ADDRESS_RANGE, 0) <= 0) {
		nok("writing 0 was rejected; it should reset to the default");
	} else if (get_msr(v, MSR_PVM_LINEAR_ADDRESS_RANGE, &back) != 1) {
		nok("unreadable after writing 0");
	} else if (back != saved) {
		nok("0 gave %#llx, not the default %#llx",
		    (unsigned long long)back, (unsigned long long)saved);
	} else {
		ok("back to %#llx", (unsigned long long)back);
	}
}

/* --- MSR_PVM_VCPU_STRUCT, and the memslot under it -------------------- */

static void test_vcpu_struct(struct vm *v)
{
	uint64_t back = 0;

	current_case = "pvm/vcpu-struct/unaligned";
	if (set_msr(v, MSR_PVM_VCPU_STRUCT, PVCS_GPA + 8) > 0)
		nok("an unaligned PVCS address was accepted");
	else
		ok("rejected");

	current_case = "pvm/vcpu-struct/valid";
	if (set_msr(v, MSR_PVM_VCPU_STRUCT, PVCS_GPA) <= 0)
		nok("a page-aligned PVCS inside a memslot was rejected");
	else if (get_msr(v, MSR_PVM_VCPU_STRUCT, &back) != 1 || back != PVCS_GPA)
		nok("read back %#llx, wrote %#llx",
		    (unsigned long long)back, (unsigned long long)PVCS_GPA);
	else
		ok("pinned at %#llx", (unsigned long long)PVCS_GPA);

	/*
	 * Deliberately accepted: see the comment on this case in
	 * pvm_set_msr().  What must not happen is the host noticing later
	 * in a way that takes it down rather than the guest.
	 */
	current_case = "pvm/vcpu-struct/unbacked-accepted";
	if (set_msr(v, MSR_PVM_VCPU_STRUCT,
		    GUEST_PHYS_BASE + GUEST_MEM_SIZE + 0x10000) <= 0)
		nok("a PVCS with no memslot was rejected at set time; the "
		    "restore-before-memory-regions path depends on it being "
		    "stored and refreshed later");
	else
		ok("stored, to be resolved by KVM_REQ_GPC_REFRESH");

	current_case = "pvm/vcpu-struct/zero-clears";
	if (set_msr(v, MSR_PVM_VCPU_STRUCT, 0) <= 0)
		nok("clearing the PVCS was rejected");
	else
		ok("cleared");

	/* Leave it valid for the churn test. */
	set_msr(v, MSR_PVM_VCPU_STRUCT, PVCS_GPA);
}

/* --- MSR_PVM_EVENT_ENTRY ---------------------------------------------- */

static void test_event_entry(struct vm *v)
{
	/*
	 * The entry has to be canonical at +0, +256 and +512, because the
	 * three entry points live at those offsets.  An address just below
	 * the canonical hole is canonical itself and not canonical at +256,
	 * which is the case a single check would miss.
	 */
	static const struct {
		const char *why;
		uint64_t val;
	} bad[] = {
		{ "non-canonical",		0x0000800000000000ULL },
		{ "canonical, +256 is not",	0x00007ffffffffff0ULL },
		{ "canonical, +512 is not",	0x00007ffffffffe10ULL },
	};
	size_t i;

	for (i = 0; i < sizeof(bad) / sizeof(bad[0]); i++) {
		char name[128];

		snprintf(name, sizeof(name), "pvm/event-entry/reject-%zu", i);
		current_case = name;
		if (set_msr(v, MSR_PVM_EVENT_ENTRY, bad[i].val) > 0)
			nok("%#llx (%s) was accepted",
			    (unsigned long long)bad[i].val, bad[i].why);
		else
			ok("%s rejected", bad[i].why);
	}
}

/* --- the whole PVM MSR window ----------------------------------------- */

static void test_msr_window(struct vm *v)
{
	static const uint64_t values[] = { 0, 1, ~0ULL, 0x0000800000000000ULL,
					   0xffffffff80000000ULL };
	uint32_t msr;
	size_t i;
	int survived = 1;

	/*
	 * Every MSR in the PVM range, including the two that were deprecated
	 * and the unassigned tail, with values chosen to be wrong.  Nothing
	 * here asserts an outcome -- some of these are legitimately accepted
	 * -- only that the host is still answering ioctls afterwards.
	 */
	current_case = "pvm/msr-window/sweep";
	for (msr = PVM_VIRTUAL_MSR_BASE;
	     msr <= PVM_VIRTUAL_MSR_BASE + PVM_VIRTUAL_MSR_MAX_NR; msr++) {
		for (i = 0; i < sizeof(values) / sizeof(values[0]); i++) {
			if (set_msr(v, msr, values[i]) == -EINVAL) {
				survived = 0;
				nok("KVM_SET_MSRS(%#x) failed with EINVAL, "
				    "which is the ioctl refusing the request "
				    "rather than the vendor refusing the value",
				    msr);
				break;
			}
		}
	}
	if (survived) {
		uint64_t dummy;

		if (get_msr(v, MSR_PVM_LINEAR_ADDRESS_RANGE, &dummy) < 0)
			nok("the vCPU stopped answering after the sweep");
		else
			ok("%d MSRs x %zu values, host still answering",
			   PVM_VIRTUAL_MSR_MAX_NR + 1,
			   sizeof(values) / sizeof(values[0]));
	}

	/* Put the range back so later cases start from a known state. */
	set_msr(v, MSR_PVM_LINEAR_ADDRESS_RANGE, 0);
}

/* --- memslot churn under a pinned PVCS -------------------------------- */

/*
 * Shared with the vCPU thread.  volatile because the churn loop spins on
 * run_entered waiting for the thread to be under way -- without it, and
 * without a barrier, the compiler is free to hoist the load out of the
 * loop and spin forever on a stale zero.
 */
static volatile int churn_stop;
static volatile int run_entered, run_failed;
static volatile int run_last_errno, run_last_reason;
static volatile uint32_t run_last_suberror;

static const char *exit_reason_name(int r)
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

static void *vcpu_thread(void *arg)
{
	struct vm *v = arg;
	int i;

	/*
	 * The vCPU has no valid PVM state, so every entry ends badly -- a
	 * triple fault, or an ioctl error.  That is fine and is not what is
	 * being tested: the point is to have entries in flight while the
	 * memory map changes under the pinned PVCS, so that
	 * pvm_vcpu_gpc_refresh() runs against a moving target.
	 *
	 * Which is exactly why the outcome is counted.  A KVM_RUN that is
	 * refused by the ioctl before the vCPU is ever entered would make
	 * this test pass without churning anything, and the pass would look
	 * identical.
	 */
	for (i = 0; i < 100000 && !churn_stop; i++) {
		if (ioctl(v->vcpu, KVM_RUN, 0) < 0) {
			run_failed++;
			run_last_errno = errno;
		} else {
			run_entered++;
			run_last_reason = v->run->exit_reason;
			if (run_last_reason == KVM_EXIT_INTERNAL_ERROR)
				run_last_suberror = v->run->internal.suberror;
		}
	}
	return NULL;
}

static void test_memslot_churn(struct vm *v)
{
	pthread_t t;
	int i, err = 0;

	current_case = "pvm/memslot-churn";

	if (set_msr(v, MSR_PVM_VCPU_STRUCT, PVCS_GPA) <= 0) {
		nok("could not pin a PVCS to churn under");
		return;
	}

	churn_stop = 0;
	run_entered = run_failed = 0;
	if (pthread_create(&t, NULL, vcpu_thread, v) != 0) {
		nok("pthread_create: %s", strerror(errno));
		return;
	}

	/*
	 * Wait for the vCPU thread to actually be entering before touching
	 * the memory map.  The churn is 200 ioctls and finishes in well
	 * under a scheduling quantum, so without this the whole loop can run
	 * and set churn_stop before the thread is first scheduled -- and
	 * then the case passes having tested nothing.  That is not
	 * hypothetical; it is what this test did until it was made to count
	 * its own entries.
	 */
	for (i = 0; i < 100000 && run_entered == 0 && run_failed == 0; i++)
		usleep(100);
	if (run_entered == 0 && run_failed == 0) {
		churn_stop = 1;
		pthread_join(t, NULL);
		nok("the vCPU thread never made a KVM_RUN call");
		return;
	}

	/*
	 * Delete and re-add the slot the PVCS lives in, then move it, while
	 * the vCPU thread is entering.  Deleting it unpins the PVCS page
	 * from under a vCPU that may be about to use it.
	 */
	for (i = 0; i < 50; i++) {
		if (set_memslot(v, 0, GUEST_PHYS_BASE, 0, v->mem, 0)) {
			err = errno;
			break;
		}
		if (set_memslot(v, 0, GUEST_PHYS_BASE, GUEST_MEM_SIZE, v->mem, 0)) {
			err = errno;
			break;
		}
		/* Move it somewhere else and back. */
		if (set_memslot(v, 0, GUEST_PHYS_BASE, 0, v->mem, 0) ||
		    set_memslot(v, 0, GUEST_PHYS_BASE + GUEST_MEM_SIZE,
				GUEST_MEM_SIZE, v->mem, 0)) {
			err = errno;
			break;
		}
		if (set_memslot(v, 0, GUEST_PHYS_BASE + GUEST_MEM_SIZE, 0,
				v->mem, 0) ||
		    set_memslot(v, 0, GUEST_PHYS_BASE, GUEST_MEM_SIZE, v->mem, 0)) {
			err = errno;
			break;
		}
	}

	churn_stop = 1;
	pthread_join(t, NULL);

	if (err) {
		nok("memslot update failed after %d rounds: %s", i, strerror(err));
		return;
	}

	/* Still alive, still answering. */
	if (set_msr(v, MSR_PVM_VCPU_STRUCT, PVCS_GPA) <= 0) {
		nok("the vCPU stopped accepting a PVCS after the churn");
		return;
	}

	/*
	 * The churn is only a test of the refresh path if the vCPU actually
	 * entered.  If every KVM_RUN was refused outright then nothing was
	 * in flight and this case proves nothing -- say so rather than
	 * printing a green line.
	 */
	if (run_entered == 0) {
		nok("%d rounds, but not one KVM_RUN entered the vCPU "
		    "(%d refused, last errno %d): nothing was in flight, so "
		    "this case tested nothing",
		    i, run_failed, run_last_errno);
		return;
	}

	/*
	 * A vCPU that was never given valid PVM state cannot get far, so an
	 * unhappy exit is the expected outcome and is not the assertion.
	 * It is reported by name because "17" in a log six months from now
	 * is not information, and because an internal error's suberror says
	 * whether KVM gave up in the emulator or somewhere less ordinary.
	 */
	if (run_last_reason == KVM_EXIT_INTERNAL_ERROR)
		ok("%d rounds; %d entries (last exit %s/%u), %d refused",
		   i, run_entered, exit_reason_name(run_last_reason),
		   run_last_suberror, run_failed);
	else
		ok("%d rounds; %d entries (last exit %s), %d refused "
		   "(last errno %d)", i, run_entered,
		   exit_reason_name(run_last_reason), run_failed,
		   run_last_errno);
}

/* --- a PVCS the host cannot pin, taken all the way to KVM_RUN --------- */

/*
 * virt-pvm/linux#7: a cross-host snapshot restore panicked the *host* with
 * a NULL dereference in pvm_vcpu_run(), reading pvcs->event_flags with the
 * guard on pvm->msr_vcpu_struct -- the guest physical address -- rather
 * than on pvm->pvcs, the mapped pointer.  A VMM that restores MSRs before
 * it adds memory regions sets one without the other.
 *
 * The shape that crashed is still in the code, deliberately: the fix is
 * the WARN_ON_ONCE plus triple fault at the top of pvm_vcpu_run(), and
 * before that the KVM_REQ_GPC_REFRESH that pvm_set_msr() raises when the
 * pin fails.  What these two cases check is that the fail-closed path
 * really is the one taken, from the VMM's side, for both ways of making
 * the pin fail: no memslot at all, and a memslot whose backing memory
 * cannot be pinned for write.
 *
 * The assertion in both is the same and it is about the host: the ioctls
 * keep working afterwards.  A host that oopsed would not answer at all.
 */
static void run_with_unpinnable_pvcs(struct vm *v, const char *what,
				     uint64_t gpa)
{
	int i, entered = 0, refused = 0, last_errno = 0, last_reason = -1;

	if (set_msr(v, MSR_PVM_VCPU_STRUCT, gpa) <= 0) {
		nok("%s: setting the PVCS was rejected outright; the "
		    "restore-before-memory-regions path depends on it being "
		    "stored and resolved later", what);
		return;
	}

	for (i = 0; i < 20; i++) {
		if (ioctl(v->vcpu, KVM_RUN, 0) < 0) {
			refused++;
			last_errno = errno;
		} else {
			entered++;
			last_reason = v->run->exit_reason;
		}
	}

	/* Back to something sane, and prove the vCPU still answers. */
	if (set_msr(v, MSR_PVM_VCPU_STRUCT, 0) <= 0) {
		nok("%s: the vCPU stopped answering after %d entries",
		    what, entered);
		return;
	}

	if (entered == 0 && refused == 0) {
		nok("%s: KVM_RUN was never called", what);
		return;
	}

	ok("%s: %d entries (last exit %s), %d refused (last errno %d), "
	   "host still answering", what, entered,
	   last_reason < 0 ? "none" : exit_reason_name(last_reason),
	   refused, last_errno);
}

static void test_unpinnable_pvcs(struct vm *v)
{
	void *ro;

	current_case = "pvm/vcpu-struct/unbacked-run";
	run_with_unpinnable_pvcs(v, "no memslot behind the PVCS",
				 GUEST_PHYS_BASE + GUEST_MEM_SIZE + 0x10000);

	/*
	 * A memslot whose userspace mapping is read-only.  PVM pins the PVCS
	 * page with FOLL_WRITE -- it writes the event frame into it -- so the
	 * pin fails even though the memslot itself is perfectly valid.
	 */
	current_case = "pvm/vcpu-struct/readonly-run";
	ro = mmap(NULL, GUEST_MEM_SIZE, PROT_READ,
		  MAP_PRIVATE | MAP_ANONYMOUS | MAP_NORESERVE, -1, 0);
	if (ro == MAP_FAILED) {
		nok("mmap PROT_READ: %s", strerror(errno));
		return;
	}
	if (set_memslot(v, 1, GUEST_PHYS_BASE + 2 * GUEST_MEM_SIZE,
			GUEST_MEM_SIZE, ro, 0)) {
		nok("adding a read-only-backed memslot: %s", strerror(errno));
		munmap(ro, GUEST_MEM_SIZE);
		return;
	}
	run_with_unpinnable_pvcs(v, "PVCS on read-only backing memory",
				 GUEST_PHYS_BASE + 2 * GUEST_MEM_SIZE);
	set_memslot(v, 1, GUEST_PHYS_BASE + 2 * GUEST_MEM_SIZE, 0, ro, 0);
	munmap(ro, GUEST_MEM_SIZE);
}

/* --- does the host's PKRU leak into guest state? ---------------------- */

/*
 * PVM runs the guest at hardware CPL3 on the host's own PKRU, so
 * pvm_load_guest_xsave_state() forces PKRU to 0 before entry -- otherwise
 * a host process with a restrictive PKRU would deny the guest access to
 * its own pages -- and restores the host's value on exit.
 *
 * The trouble is the order.  Common KVM brackets ->vcpu_run() with
 * kvm_load_guest_pkru() / kvm_load_host_pkru(), and the latter does
 *
 *     vcpu->arch.pkru = rdpkru();
 *
 * which runs *after* PVM's wrapper has already put the host's value back.
 * So vcpu->arch.pkru -- the guest's architectural PKRU, what KVM_GET_XSAVE
 * hands the VMM and what goes into a snapshot -- ends up holding the
 * host's.
 *
 * Both halves of the invariant are at stake: the host's PKRU must not
 * restrict the guest (the wrapper sees to that) and must not leak into it
 * (this is where it does).  The bracketing only runs when the guest has
 * XCR0.PKRU or CR4.PKE set, so the test sets CR4.PKE the way a Linux guest
 * with PKU advertised would.
 */
static uint32_t host_pkru(void)
{
	uint32_t eax, edx, ecx = 0;

	/* rdpkru */
	asm volatile(".byte 0x0f,0x01,0xee" : "=a"(eax), "=d"(edx) : "c"(ecx));
	return eax;
}

static void cpuid_count(uint32_t leaf, uint32_t sub, uint32_t r[4])
{
	asm volatile("cpuid"
		     : "=a"(r[0]), "=b"(r[1]), "=c"(r[2]), "=d"(r[3])
		     : "a"(leaf), "c"(sub));
}

#define XFEATURE_PKRU 9

/*
 * The same rule the SPEC_CTRL family is held to: a feature may only be
 * advertised if the state behind it is actually handled.  PVM runs the
 * guest at hardware CPL3 on the host's PKRU and forces it to zero for the
 * duration, so there is no guest architectural PKRU at all -- one
 * register is being asked to be three things at once (the host's, the
 * guest's, and PVM's own supervisor isolation).
 */
static void test_pku_not_advertised(struct vm *v)
{
	bool pku = cpuid_has(v->cpuid, 7, 0, 2, 3);
	bool ospke = cpuid_has(v->cpuid, 7, 0, 2, 4);

	current_case = "pvm/pku-not-advertised";
	if (pku || ospke)
		nok("KVM_GET_SUPPORTED_CPUID advertises%s%s, but PVM forces "
		    "hardware PKRU to 0 while the guest runs and keeps no "
		    "guest architectural PKRU",
		    pku ? " PKU" : "", ospke ? " OSPKE" : "");
	else
		ok("neither PKU nor OSPKE advertised");
}

static void test_pkru_leak(struct vm *v)
{
	struct kvm_sregs sregs;
	struct kvm_xsave xsave;
	uint32_t r[4], offset, size, mine;
	uint32_t guest_pkru;
	int i;

	current_case = "pvm/pkru-not-leaked";

	cpuid_count(0, 0, r);
	if (r[0] < 0xd) {
		ok("no XSAVE leaf on this CPU; nothing to check");
		return;
	}
	cpuid_count(0xd, 0, r);
	if (!(r[0] & (1u << XFEATURE_PKRU))) {
		ok("the host CPU has no PKRU xstate component");
		return;
	}
	cpuid_count(0xd, XFEATURE_PKRU, r);
	size = r[0];
	offset = r[1];
	if (size < sizeof(uint32_t) || offset + size > sizeof(xsave.region)) {
		nok("PKRU xstate component is at offset %u size %u, which does "
		    "not fit the KVM_GET_XSAVE buffer", offset, size);
		return;
	}

	/* A Linux guest told it has PKU sets CR4.PKE; do the same. */
	if (ioctl(v->vcpu, KVM_GET_SREGS, &sregs) < 0) {
		nok("KVM_GET_SREGS: %s", strerror(errno));
		return;
	}
	sregs.cr4 |= X86_CR4_PKE;
	if (ioctl(v->vcpu, KVM_SET_SREGS, &sregs) < 0) {
		ok("the host refuses CR4.PKE (%s), so the PKRU bracketing "
		   "never runs", strerror(errno));
		return;
	}

	/*
	 * And confirm it stuck.  kvm_load_{guest,host}_pkru() only run when
	 * the guest has XCR0.PKRU or CR4.PKE, so a CR4 write that was quietly
	 * dropped would make this case pass while testing nothing -- the same
	 * trap the memslot churn case fell into.
	 */
	memset(&sregs, 0, sizeof(sregs));
	if (ioctl(v->vcpu, KVM_GET_SREGS, &sregs) < 0) {
		nok("KVM_GET_SREGS after setting CR4.PKE: %s", strerror(errno));
		return;
	}
	if (!(sregs.cr4 & X86_CR4_PKE)) {
		nok("CR4.PKE did not stick (cr4=%#llx), so the PKRU bracketing "
		    "never ran and this case proved nothing",
		    (unsigned long long)sregs.cr4);
		return;
	}

	for (i = 0; i < 5; i++)
		ioctl(v->vcpu, KVM_RUN, 0);

	memset(&xsave, 0, sizeof(xsave));
	if (ioctl(v->vcpu, KVM_GET_XSAVE, &xsave) < 0) {
		nok("KVM_GET_XSAVE: %s", strerror(errno));
		return;
	}
	memcpy(&guest_pkru, (char *)xsave.region + offset, sizeof(guest_pkru));
	mine = host_pkru();

	if (guest_pkru != 0 && guest_pkru == mine) {
		nok("the guest's saved PKRU is %#x, which is this process's own "
		    "PKRU: the host's value is being handed to the VMM as guest "
		    "state and would be written into a snapshot", guest_pkru);
		return;
	}
	/*
	 * A clean result here is worth less than it looks.  This vCPU has no
	 * PVM state, so pvm_vcpu_run() returns early on non_pvm_mode and the
	 * PVM wrappers that put the host's PKRU back never run -- which is
	 * exactly the step that would make the core's
	 * "vcpu->arch.pkru = rdpkru()" pick up the host's value.  So this
	 * shows the core's own bracketing round-trips correctly; it does not
	 * clear PVM of the leak, which needs a vCPU that really enters.
	 */
	ok("guest PKRU %#x, host PKRU %#x (vCPU never left non-PVM mode, so "
	   "the PVM wrappers did not run)", guest_pkru, mine);
}

/* --- the CPL3 invariants, checked from the VMM's side ------------------ */

/*
 * These reference Documentation/virt/kvm/x86/pvm-invariants.rst by name.
 * The point of doing it here rather than as free-standing checks is that
 * the document is the contract: if an invariant is restated, the test
 * that carries its number is the one that has to change with it.
 */
static void test_invariants(struct vm *v)
{
	bool host_nx, host_la57;
	uint32_t r[4];

	/*
	 * M4: a guest kernel page and a guest user page are both USER to the
	 * hardware, so the guest-kernel-executes-guest-user case is blocked
	 * by setting NX on the user mapping.  With no host NX there is no
	 * substitute, and the document says such a host must be rejected at
	 * module load rather than silently degraded.  So SMEP may only be
	 * advertised where NX exists -- if it were advertised without,
	 * the guest would be told it has a protection nothing implements.
	 */
	current_case = "pvm/invariant-M4-smep-needs-nx";
	cpuid_count(0x80000001, 0, r);
	host_nx = r[3] & (1u << 20);
	if (cpuid_has(v->cpuid, 7, 0, 1, 7) && !host_nx)
		nok("SMEP is advertised to the guest but the host has no NX; "
		    "invariant M4 has nothing to emulate it with");
	else
		ok("SMEP advertised=%d, host NX=%d",
		   cpuid_has(v->cpuid, 7, 0, 1, 7), host_nx);

	/*
	 * M4's other half, and the reason SMAP is not in the same sentence:
	 * there is no equivalent trick for supervisor-mode *access*
	 * prevention, so SMAP must not be advertised at all.
	 */
	current_case = "pvm/invariant-M4-no-smap";
	if (cpuid_has(v->cpuid, 7, 0, 1, 20))
		nok("SMAP is advertised, but PVM emulates no equivalent: the "
		    "guest runs at CPL3 and hardware SMAP cannot separate its "
		    "two modes");
	else
		ok("SMAP not advertised");

	/*
	 * M5: host and guest share a hardware CR3, so the shadow tree has the
	 * host's depth.  A guest may only be offered LA57 where the host has
	 * it.
	 */
	current_case = "pvm/invariant-M5-la57-matches-host";
	cpuid_count(7, 0, r);
	host_la57 = r[2] & (1u << 16);
	if (cpuid_has(v->cpuid, 7, 0, 2, 16) && !host_la57)
		nok("LA57 is advertised to the guest but the host is 4-level; "
		    "the shadow root level cannot match the host's");
	else
		ok("LA57 advertised=%d, host LA57=%d",
		   cpuid_has(v->cpuid, 7, 0, 2, 16), host_la57);

	/*
	 * The host PKRU half of the CPL3 argument: the guest runs at CPL3 on
	 * the host's PKRU, so pvm_load_guest_xsave_state() forces it to 0
	 * before entry -- otherwise a host process with a restrictive PKRU
	 * would deny the guest access to its own pages.
	 *
	 * That path is only exercised when the host PKRU is actually
	 * restrictive.  On a normal Linux host it is init_pkru
	 * (0x55555554), which is why every guest run in this testbed already
	 * exercises it and they all work.  This case exists to notice if
	 * that stops being true, because then the coverage claim would be
	 * vacuous rather than satisfied.
	 */
	current_case = "pvm/invariant-host-pkru-is-restrictive";
	cpuid_count(7, 0, r);
	if (!(r[2] & (1u << 4))) {
		ok("no OSPKE on the host; nothing to restrict with");
	} else if (host_pkru() == 0) {
		nok("this process's PKRU is 0, so every guest run has been "
		    "exercising the easy case: the wrapper that forces PKRU "
		    "to 0 for the guest is never doing anything");
	} else {
		ok("host PKRU is %#x, so the forced-to-zero path is live",
		   host_pkru());
	}
}

/* --- is this even a PVM host? ----------------------------------------- */

static int is_pvm_host(void)
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

int main(void)
{
	struct vm v = {};

	if (!is_pvm_host()) {
		printf("1..0 # SKIP kvm_pvm is not the loaded vendor module\n");
		return 0;
	}

	vm_setup(&v);

	test_linear_range(&v);
	test_vcpu_struct(&v);
	test_event_entry(&v);
	test_msr_window(&v);
	test_unpinnable_pvcs(&v);
	test_pku_not_advertised(&v);
	test_invariants(&v);
	test_pkru_leak(&v);
	test_memslot_churn(&v);

	vm_teardown(&v);

	printf("1..%d\n", pass + fail);
	printf("PVMHOSTTEST-RESULT: %s pass=%d fail=%d\n",
	       fail ? "fail" : "ok", pass, fail);
	return fail ? 1 : 0;
}
