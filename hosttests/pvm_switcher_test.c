// SPDX-License-Identifier: GPL-2.0
/*
 * The switcher's direct-switch path, driven from the VMM with a guest that
 * does nothing else.
 *
 * Invariants S1, S2 and S4 in Documentation/virt/kvm/x86/pvm-invariants.rst
 * are all properties of the supervisor-to-user direct switch in
 * arch/x86/entry/entry_64_switcher.S.  The Coverage section used to call
 * them "not checkable from outside", which was wrong: every input that
 * path trusts comes from the PVCS, and the PVCS is a guest page.  A guest
 * that wants to attack the host writes the hostile value there and issues
 * the return-to-user synthetic instruction.  So can we.
 *
 *   S1  SYSRET with a non-canonical RCX/RIP raises #GP in *kernel* space on
 *       Intel, on a stack the guest controls, which is a host takeover
 *       primitive.  The switcher canonicalises RCX and compares before
 *       committing to sysretq, falling back to IRET.  Case: put a
 *       non-canonical rip in the PVCS and return to user with it.
 *
 *   S2  EFLAGS entering the guest is masked to SWITCH_ENTER_EFLAGS_ALLOWED
 *       and forced to SWITCH_ENTER_EFLAGS_FIXED.  IOPL, VM, VIF and VIP
 *       must never be settable by the guest.  Case: ask for all of them and
 *       have the guest report the RFLAGS it actually got.
 *
 *   S4  The fast path does not validate PVCS::user_cs/user_ss; it assumes
 *       them, and is only taken when they are exactly
 *       (__USER_DS << 16) | __USER_CS.  Case: ask for a different pair and
 *       check the guest's architectural CS/SS came from it, which only the
 *       hypervisor's full emulation sets.
 *
 * What this cannot see from userspace is *which* path serviced a given
 * transition: a direct switch produces no KVM exit, and neither does a
 * hypervisor-emulated one.  The cases are built so the direct switch is
 * the live path -- the guest bounces between its two modes several times
 * first, which is what gets both shadow roots into prev_roots[] and clears
 * SWITCH_FLAGS_NO_DS_CR3 -- and then assert on the outcome.  A host that
 * took the hostile value through the fast path would not be here to print
 * the result.
 *
 * The guest is 30 or so hand-assembled bytes per mode.  It has no BIOS, no
 * IDT and no GDT of its own: PVM emulates segmentation, and the VMM hands
 * the vCPU a long-mode state directly, which is what try_to_convert_to_-
 * pvm_mode() turns into a PVM guest.
 */

#include <stddef.h>

#include "harness.h"

#include <signal.h>

/*
 * The host's own selectors.  The switcher compares PVCS::user_cs/user_ss
 * against this exact pair, because the guest at CPL3 runs on the host's
 * GDT.  They are ABI on x86_64 and have been since the beginning.
 */
#define HOST_USER_CS	0x33
#define HOST_USER_DS	0x2b
#define GOOD_SEL	(((uint32_t)HOST_USER_DS << 16) | HOST_USER_CS)

/*
 * Offsets into struct pvm_vcpu_struct, taken from the struct rather than
 * written out: the guest here builds its PVCS as raw bytes, so a field moving
 * would otherwise leave these pointing somewhere plausible and wrong.
 */
#define PVCS_USER_CS	offsetof(struct pvm_vcpu_struct, user_cs)
#define PVCS_EVENT_VEC	offsetof(struct pvm_vcpu_struct, event_vector)
#define PVCS_EFLAGS	offsetof(struct pvm_vcpu_struct, eflags)
#define PVCS_RIP	offsetof(struct pvm_vcpu_struct, rip)

/* Guest physical layout, all offsets from GUEST_PHYS_BASE. */
#define O_PML4		0x0000
#define O_PDP_K		0x1000
#define O_PD_K		0x2000
#define O_PT_K		0x3000
#define O_PDP_U		0x4000
#define O_PD_U		0x5000
#define O_PT_U		0x6000
#define O_PVCS		0x7000
#define O_CTRL		0x8000
#define O_SCODE		0x9000
#define O_EVENT		0xa000
#define O_UCODE		0xb000
#define O_USTACK	0xc000
#define O_MAPPED_PAGES	13	/* everything above, mapped into the kernel */

/* Two pages with no memslot behind them: the guest's only output device. */
#define MMIO_REPORT	0xd0000000UL	/* written from user mode */
#define MMIO_EVENT	0xd0001000UL	/* written from the event handlers */

/* User virtual addresses.  Lower half, so always an allowed guest VA. */
#define UVA_BASE	0x40000000UL
#define UVA_CODE	(UVA_BASE + 0x0000)
#define UVA_STACK_TOP	(UVA_BASE + 0x2000)
#define UVA_MMIO	(UVA_BASE + 0x2000)

/* Offsets into the control page the guest reads its next PVCS values from. */
#define CTRL_RIP	0x00	/* u64 */
#define CTRL_EFLAGS	0x08	/* u32 */
#define CTRL_SEL	0x0c	/* u32: user_cs | user_ss << 16 */

#define PTE_P		(1ULL << 0)
#define PTE_RW		(1ULL << 1)
#define PTE_US		(1ULL << 2)

/* The markers the event handlers OR the PVCS event vector into. */
#define MARK_USER_EVENT		0xe7000000u
#define MARK_SUPERVISOR_EVENT	0xe8000000u

struct guest {
	uint8_t *mem;		/* host mapping of GUEST_PHYS_BASE */
	uint64_t kva;		/* guest kernel VA of GUEST_PHYS_BASE */
	uint64_t smod_entry;	/* also the LSTAR entry */
	uint64_t retu_rip;	/* the SYSCALL that returns to user mode */
	uint64_t event_entry;
};

/* --- hand assembly ----------------------------------------------------- */

struct asmbuf {
	uint8_t *p;
	uint64_t va;		/* VA of the next byte to be emitted */
};

static void emit(struct asmbuf *a, const void *bytes, size_t n)
{
	memcpy(a->p, bytes, n);
	a->p += n;
	a->va += n;
}

#define EMIT(a, ...) do {						\
	const uint8_t _b[] = { __VA_ARGS__ };				\
	emit((a), _b, sizeof(_b));					\
} while (0)

/* movabs $imm64, %reg -- REX.W B8+r imm64 */
static void emit_movabs(struct asmbuf *a, int reg, uint64_t imm)
{
	uint8_t b[10] = { 0x48, (uint8_t)(0xb8 + reg) };

	memcpy(b + 2, &imm, 8);
	emit(a, b, sizeof(b));
}

#define GPR_RAX 0
#define GPR_RDX 2
#define GPR_RSP 4
#define GPR_RSI 6
#define GPR_RDI 7

/* --- the guest ---------------------------------------------------------- */

/*
 * Supervisor mode.  Entered twice over: once because KVM_SET_REGS points
 * RIP here, and thereafter because it is MSR_LSTAR, so a SYSCALL from user
 * mode arrives here too.
 *
 * It copies three fields from the control page into the PVCS and returns
 * to user mode.  That indirection is the whole point: the VMM can plant a
 * hostile rip, EFLAGS or selector pair between two guest exits, without
 * the guest having to know which case is running, and the switcher's
 * umod->smod direction -- which overwrites PVCS::rip with the user return
 * address -- cannot undo it, because the copy happens after it.
 */
static void build_smod_code(struct guest *g)
{
	struct asmbuf a = { g->mem + O_SCODE, g->kva + O_SCODE };
	uint64_t pvcs = g->kva + O_PVCS;
	uint64_t ctrl = g->kva + O_CTRL;

	g->smod_entry = a.va;

	/*
	 * The user RSP for the return.  On both the direct switch and the
	 * emulated path, user mode resumes on the RSP supervisor mode had,
	 * so this is also where the user stack gets established.
	 */
	emit_movabs(&a, GPR_RSP, UVA_STACK_TOP);
	emit_movabs(&a, GPR_RDI, pvcs);
	emit_movabs(&a, GPR_RSI, ctrl);

	EMIT(&a, 0x48, 0x8b, 0x46, CTRL_RIP);		/* mov CTRL_RIP(%rsi),%rax */
	EMIT(&a, 0x48, 0x89, 0x47, PVCS_RIP);		/* mov %rax,PVCS_RIP(%rdi) */
	EMIT(&a, 0x8b, 0x46, CTRL_EFLAGS);		/* mov CTRL_EFLAGS(%rsi),%eax */
	EMIT(&a, 0x89, 0x47, PVCS_EFLAGS);		/* mov %eax,PVCS_EFLAGS(%rdi) */
	EMIT(&a, 0x8b, 0x46, CTRL_SEL);			/* mov CTRL_SEL(%rsi),%eax */
	EMIT(&a, 0x89, 0x47, PVCS_USER_CS);		/* mov %eax,PVCS_USER_CS(%rdi) */

	/*
	 * The return-to-user synthetic instruction: a SYSCALL at
	 * MSR_PVM_RETU_RIP.  The hypervisor adds the 2 itself.
	 */
	g->retu_rip = a.va;
	EMIT(&a, 0x0f, 0x05);				/* syscall */
	EMIT(&a, 0x0f, 0x0b);				/* ud2 */
}

/*
 * User mode.  Reports the RFLAGS it was given -- which is the S2 assertion
 * and doubles as "the transition happened at all" for the others -- and
 * syscalls back into supervisor mode.  It never returns from that syscall:
 * supervisor mode goes round again and re-enters user mode at whatever
 * PVCS::rip the control page now says.
 */
static void build_umod_code(struct guest *g)
{
	struct asmbuf a = { g->mem + O_UCODE, UVA_CODE };

	EMIT(&a, 0x9c);					/* pushfq */
	EMIT(&a, 0x58);					/* pop %rax */
	emit_movabs(&a, GPR_RDX, UVA_MMIO);
	EMIT(&a, 0x89, 0x02);				/* mov %eax,(%rdx) */
	EMIT(&a, 0x0f, 0x05);				/* syscall */
	EMIT(&a, 0x0f, 0x0b);				/* ud2 */
}

/*
 * The event handlers, at MSR_PVM_EVENT_ENTRY and +512 as the ABI requires.
 * Both run in supervisor mode.  Each reports the PVCS event vector with a
 * marker saying which one ran, and then spins: the VMM stops calling
 * KVM_RUN once it has seen the report, so the spin is never executed twice.
 */
static void build_event_code(struct guest *g, uint64_t off, uint32_t marker)
{
	struct asmbuf a = { g->mem + O_EVENT + off, g->kva + O_EVENT + off };
	uint8_t or_imm[5] = { 0x0d };

	emit_movabs(&a, GPR_RDI, g->kva + O_PVCS);
	EMIT(&a, 0x0f, 0xb7, 0x47, PVCS_EVENT_VEC);	/* movzwl vec(%rdi),%eax */
	memcpy(or_imm + 1, &marker, 4);
	emit(&a, or_imm, sizeof(or_imm));		/* or $marker,%eax */
	emit_movabs(&a, GPR_RDX, g->kva + 0x10000);	/* the event MMIO page */
	EMIT(&a, 0x89, 0x02);				/* mov %eax,(%rdx) */
	EMIT(&a, 0xeb, 0xfe);				/* 1: jmp 1b */
}

/* --- page tables -------------------------------------------------------- */

static void put_pte(struct guest *g, uint64_t table, unsigned index, uint64_t v)
{
	uint64_t *t = (uint64_t *)(g->mem + table);

	t[index] = v;
}

#define PT_INDEX(va, level) (((va) >> (12 + 9 * ((level) - 1))) & 0x1ff)

/*
 * One 4-level tree for both of the guest's modes.  PVM builds two shadow
 * roots from it and tells them apart by the U/S bit, which is why the two
 * halves must differ in exactly that: the kernel side without _PAGE_USER,
 * the user side with it.
 */
static void build_page_tables(struct guest *g)
{
	uint64_t base = GUEST_PHYS_BASE;
	unsigned i;

	put_pte(g, O_PML4, PT_INDEX(g->kva, 4), (base + O_PDP_K) | PTE_P | PTE_RW);
	put_pte(g, O_PDP_K, PT_INDEX(g->kva, 3), (base + O_PD_K) | PTE_P | PTE_RW);
	put_pte(g, O_PD_K, PT_INDEX(g->kva, 2), (base + O_PT_K) | PTE_P | PTE_RW);

	for (i = 0; i < O_MAPPED_PAGES; i++)
		put_pte(g, O_PT_K, PT_INDEX(g->kva, 1) + i,
			(base + i * 0x1000) | PTE_P | PTE_RW);

	/* kva + 0x10000: the MMIO page the event handlers write. */
	put_pte(g, O_PT_K, PT_INDEX(g->kva + 0x10000, 1), MMIO_EVENT | PTE_P | PTE_RW);

	put_pte(g, O_PML4, PT_INDEX(UVA_BASE, 4), (base + O_PDP_U) | PTE_P | PTE_RW | PTE_US);
	put_pte(g, O_PDP_U, PT_INDEX(UVA_BASE, 3), (base + O_PD_U) | PTE_P | PTE_RW | PTE_US);
	put_pte(g, O_PD_U, PT_INDEX(UVA_BASE, 2), (base + O_PT_U) | PTE_P | PTE_RW | PTE_US);

	put_pte(g, O_PT_U, PT_INDEX(UVA_CODE, 1), (base + O_UCODE) | PTE_P | PTE_RW | PTE_US);
	put_pte(g, O_PT_U, PT_INDEX(UVA_BASE + 0x1000, 1),
		(base + O_USTACK) | PTE_P | PTE_RW | PTE_US);
	put_pte(g, O_PT_U, PT_INDEX(UVA_MMIO, 1), MMIO_REPORT | PTE_P | PTE_RW | PTE_US);
}

/* --- vCPU state --------------------------------------------------------- */

static void set_seg(struct kvm_segment *s, uint16_t sel, uint8_t type,
		    int l, int db)
{
	memset(s, 0, sizeof(*s));
	s->selector = sel;
	s->limit = 0xfffff;
	s->type = type;
	s->present = 1;
	s->dpl = 0;
	s->s = 1;
	s->l = l;
	s->db = db;
	s->g = 1;
}

/*
 * Straight into 64-bit mode with paging on.  PVM comes out of reset in
 * "non-PVM mode", an emulated stand-in for the real-mode boot a VMM would
 * otherwise have to do; setting a long-mode CS with DPL 0 is what makes
 * try_to_convert_to_pvm_mode() take the vCPU out of it.
 */
static int vcpu_enter_long_mode(struct vm *v, struct guest *g)
{
	struct kvm_sregs s;
	struct kvm_regs r;

	if (ioctl(v->vcpu, KVM_GET_SREGS, &s) < 0)
		return -errno;

	s.cr0 = 0x80050033;	/* PG | AM | WP | NE | ET | MP | PE */
	s.cr3 = GUEST_PHYS_BASE + O_PML4;
	s.cr4 = 1UL << 5;	/* PAE */
	s.efer = 0xd01;		/* NXE | LMA | LME | SCE */

	set_seg(&s.cs, 0x10, 0xb, 1, 0);	/* exec/read, accessed, 64-bit */
	set_seg(&s.ds, 0x18, 0x3, 0, 1);
	s.es = s.fs = s.gs = s.ss = s.ds;
	memset(&s.ldt, 0, sizeof(s.ldt));	/* PVM supports no LDT */
	s.tr.present = 1;
	s.tr.type = 11;

	if (ioctl(v->vcpu, KVM_SET_SREGS, &s) < 0)
		return -errno;

	memset(&r, 0, sizeof(r));
	r.rip = g->smod_entry;
	r.rflags = 0x202;
	if (ioctl(v->vcpu, KVM_SET_REGS, &r) < 0)
		return -errno;

	return 0;
}

/*
 * Where to put the guest kernel.  Any lower-half address will do -- that is
 * the whole of what a PVM guest is allowed -- so this is the base of the half
 * a real PVM guest kernel uses, picked for documentation value rather than
 * necessity.  It used to have to be decoded out of the reset value of
 * MSR_PVM_LINEAR_ADDRESS_RANGE, which no longer exists.
 */
#define KVA_BASE	(1UL << 46)

static int guest_setup(struct vm *v, struct guest *g)
{
	int r;

	memset(v->mem, 0, GUEST_MEM_SIZE);
	g->mem = v->mem;

	g->kva = KVA_BASE;

	g->event_entry = g->kva + O_EVENT;

	build_smod_code(g);
	build_umod_code(g);
	build_event_code(g, 0, MARK_USER_EVENT);
	build_event_code(g, 512, MARK_SUPERVISOR_EVENT);
	build_page_tables(g);

	if (set_msr(v, MSR_PVM_VCPU_STRUCT, GUEST_PHYS_BASE + O_PVCS) <= 0)
		return -1;
	if (set_msr(v, MSR_PVM_EVENT_ENTRY, g->event_entry) <= 0)
		return -1;
	if (set_msr(v, MSR_PVM_RETU_RIP, g->retu_rip) <= 0)
		return -1;
	if (set_msr(v, 0xc0000082 /* MSR_LSTAR */, g->smod_entry) <= 0)
		return -1;
	if (set_msr(v, 0xc0000084 /* MSR_SYSCALL_MASK */, 0) <= 0)
		return -1;

	r = vcpu_enter_long_mode(v, g);
	if (r)
		return -1;

	return 0;
}

static void ctrl_set(struct guest *g, uint64_t rip, uint32_t eflags, uint32_t sel)
{
	uint8_t *c = g->mem + O_CTRL;

	memcpy(c + CTRL_RIP, &rip, 8);
	memcpy(c + CTRL_EFLAGS, &eflags, 4);
	memcpy(c + CTRL_SEL, &sel, 4);
}

/* --- running it --------------------------------------------------------- */

struct outcome {
	int reports;		/* user-mode RFLAGS reports */
	uint32_t last_rflags;
	int events;		/* event-handler reports */
	uint32_t last_event;
	int exits;		/* KVM_RUN calls that returned */
	int stopped_by;		/* the exit reason that ended the loop, or -1 */
	int err;		/* errno if KVM_RUN failed */
};

static void alarm_handler(int sig)
{
	(void)sig;
}

/*
 * KVM_RUN until the guest reports something the caller is waiting for, or
 * until it does something else entirely.  A guest that neither exits nor
 * faults would otherwise hang the run: SIGALRM without SA_RESTART makes
 * KVM_RUN return EINTR, which is a failed case rather than a dead test.
 */
static void guest_run(struct vm *v, struct outcome *o, int want_reports,
		      int seconds)
{
	struct sigaction sa = { .sa_handler = alarm_handler };

	memset(o, 0, sizeof(*o));
	o->stopped_by = -1;

	sigemptyset(&sa.sa_mask);
	sigaction(SIGALRM, &sa, NULL);
	alarm(seconds);

	while (o->reports < want_reports) {
		if (ioctl(v->vcpu, KVM_RUN, 0) < 0) {
			o->err = errno;
			break;
		}
		o->exits++;

		if (v->run->exit_reason != KVM_EXIT_MMIO) {
			o->stopped_by = v->run->exit_reason;
			break;
		}
		if (!v->run->mmio.is_write) {
			o->stopped_by = KVM_EXIT_MMIO;
			break;
		}

		if (v->run->mmio.phys_addr == MMIO_REPORT) {
			memcpy(&o->last_rflags, v->run->mmio.data, 4);
			o->reports++;
		} else if (v->run->mmio.phys_addr == MMIO_EVENT) {
			memcpy(&o->last_event, v->run->mmio.data, 4);
			o->events++;
			break;
		} else {
			o->stopped_by = KVM_EXIT_MMIO;
			break;
		}
	}

	alarm(0);
}

static void describe(const struct outcome *o, char *buf, size_t n)
{
	snprintf(buf, n,
		 "%d report(s) (last rflags %#x), %d event(s) (last %#x), "
		 "%d exit(s), stopped by %s%s",
		 o->reports, o->last_rflags, o->events, o->last_event, o->exits,
		 o->stopped_by < 0 ? "nothing" : exit_reason_name(o->stopped_by),
		 o->err == EINTR ? ", TIMED OUT" : "");
}

/*
 * Bounce between the two modes a few times with everything well formed.
 * Two things come out of it: the harness is proved to work at all, and
 * both shadow roots end up in prev_roots[], which is what clears
 * SWITCH_FLAGS_NO_DS_CR3 and makes the direct switch the live path for
 * everything after.
 */
#define WARMUP_ROUNDS 4

static int warm_up(struct vm *v, struct guest *g, const char *what)
{
	struct outcome o;
	char desc[256];

	ctrl_set(g, UVA_CODE, 0x202, GOOD_SEL);
	guest_run(v, &o, WARMUP_ROUNDS, 10);
	if (o.reports < WARMUP_ROUNDS) {
		describe(&o, desc, sizeof(desc));
		nok("%s: the guest never got going: %s", what, desc);
		return -1;
	}
	return 0;
}

/*
 * Is the host still there?  Every case here is a negative test whose real
 * assertion is this one, and the way to ask is to keep using the vCPU.
 */
static int host_still_answering(struct vm *v)
{
	struct kvm_regs r;

	return ioctl(v->vcpu, KVM_GET_REGS, &r) == 0;
}

static void test_s1_noncanonical_rip(struct vm *v, struct guest *g)
{
	struct outcome o;
	char desc[256];

	current_case = "pvm/invariant-S1-noncanonical-eretu-rip";
	if (warm_up(v, g, "S1"))
		return;

	/*
	 * The first address above the 47-bit canonical range.  On a 5-level
	 * host the switcher canonicalises to 57 bits instead, and this is a
	 * perfectly good address there -- so the guest would simply run at
	 * it and fault on the unmapped page, which is still not a host
	 * takeover.  Either way the assertion below holds.
	 */
	ctrl_set(g, 0x0000800000000000ULL, 0x202, GOOD_SEL);
	guest_run(v, &o, 1, 10);
	describe(&o, desc, sizeof(desc));

	if (o.err == EINTR)
		nok("the vCPU never came back: %s", desc);
	else if (!host_still_answering(v))
		nok("the vCPU stopped answering ioctls: %s", desc);
	else
		ok("%s", desc);
}

static void test_s2_eflags(struct vm *v, struct guest *g)
{
	struct outcome o;
	char desc[256];
	uint32_t forbidden = 0x3000	/* IOPL */
			   | 0x20000	/* VM */
			   | 0x80000	/* VIF */
			   | 0x100000;	/* VIP */
	uint32_t got;

	current_case = "pvm/invariant-S2-eflags-sanitised";
	if (warm_up(v, g, "S2"))
		return;

	ctrl_set(g, UVA_CODE, 0x202 | forbidden, GOOD_SEL);
	guest_run(v, &o, 1, 10);
	describe(&o, desc, sizeof(desc));

	if (o.reports < 1) {
		nok("user mode never reported back: %s", desc);
		return;
	}

	got = o.last_rflags;
	if (got & forbidden)
		nok("user mode got RFLAGS %#x, which still has %#x of "
		    "IOPL/VM/VIF/VIP set", got, got & forbidden);
	else if (!(got & 0x2) || !(got & 0x200))
		nok("user mode got RFLAGS %#x, without the fixed bit or IF",
		    got);
	else
		ok("asked for %#x, got %#x", 0x202 | forbidden, got);
}

static void test_s4_selectors(struct vm *v, struct guest *g)
{
	struct outcome o;
	char desc[256];
	struct kvm_sregs before, after;
	uint32_t odd_sel = HOST_USER_CS;	/* user_cs kept, user_ss zero */

	current_case = "pvm/invariant-S4-unexpected-selectors";
	if (warm_up(v, g, "S4"))
		return;

	if (ioctl(v->vcpu, KVM_GET_SREGS, &before) < 0) {
		nok("KVM_GET_SREGS: %s", strerror(errno));
		return;
	}

	/*
	 * A null SS with the expected CS.  Not the pair the fast path tests
	 * for, so it must decline; architecturally fine in long mode, so the
	 * hypervisor's emulation can carry it through and the guest keeps
	 * running -- which is what makes the two paths tell apart.  The fast
	 * path never touches PVM's idea of the guest's CS/SS, so if it had
	 * wrongly been taken, SS below would still read as it did before.
	 */
	ctrl_set(g, UVA_CODE, 0x202, odd_sel);
	guest_run(v, &o, 1, 10);
	describe(&o, desc, sizeof(desc));

	if (!host_still_answering(v)) {
		nok("the vCPU stopped answering ioctls: %s", desc);
		return;
	}
	if (ioctl(v->vcpu, KVM_GET_SREGS, &after) < 0) {
		nok("KVM_GET_SREGS after: %s", strerror(errno));
		return;
	}

	if (o.reports < 1) {
		/*
		 * Declining into a guest fault is also a correct answer to an
		 * unexpected pair; it is not a correct answer to have run it.
		 */
		ok("the guest did not resume in user mode: %s", desc);
	} else if (after.ss.selector == before.ss.selector) {
		nok("user mode resumed with SS unchanged (%#x): the "
		    "unexpected pair was not emulated, %s",
		    before.ss.selector, desc);
	} else {
		ok("SS went %#x -> %#x through the hypervisor, %s",
		   before.ss.selector, after.ss.selector, desc);
	}
}

int main(void)
{
	struct vm v = {};
	struct guest g = {};

	if (!is_pvm_host()) {
		printf("1..0 # SKIP kvm_pvm is not the loaded vendor module\n");
		return 0;
	}

	/*
	 * One VM per case.  These leave the guest in whatever state the
	 * hostile value put it in -- faulted, spinning in an event handler,
	 * or dead -- and the next case needs a vCPU that starts from the top.
	 */
	current_case = "pvm/switcher-harness";
	vm_setup(&v);
	if (guest_setup(&v, &g)) {
		nok("could not build the guest; is this a PVM host?");
		vm_teardown(&v);
		printf("1..%d\n", pass + fail);
		printf("PVMHOSTTEST-RESULT: fail pass=%d fail=%d\n", pass, fail);
		return 1;
	}
	test_s1_noncanonical_rip(&v, &g);
	vm_teardown(&v);

	vm_setup(&v);
	if (!guest_setup(&v, &g))
		test_s2_eflags(&v, &g);
	vm_teardown(&v);

	vm_setup(&v);
	if (!guest_setup(&v, &g))
		test_s4_selectors(&v, &g);
	vm_teardown(&v);

	printf("1..%d\n", pass + fail);
	printf("PVMHOSTTEST-RESULT: %s pass=%d fail=%d\n",
	       fail ? "fail" : "ok", pass, fail);
	return fail ? 1 : 0;
}
