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

#include <sched.h>
#include <signal.h>
#include <sys/resource.h>

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
#define PVCS_CR2	offsetof(struct pvm_vcpu_struct, cr2)
#define PVCS_ERRCODE	offsetof(struct pvm_vcpu_struct, event_errcode)

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
#define O_PML4_ALT	0xd000	/* a second address space: kernel half only */
#define O_PVCS2		0xe000	/* where pvm/pvcs-alias-rebind moves the PVCS */

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
#define CTRL_PGD1	0x10	/* u64: first CR3 the pgtbl variant loads */
#define CTRL_PGD2	0x18	/* u64: second, and the one it returns to user on */
#define CTRL_PVCS	0x20	/* u64: kernel VA of the PVCS supervisor mode writes */

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
	bool pgtbl_variant;	/* supervisor mode loads two CR3s each round */
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
	uint64_t ctrl = g->kva + O_CTRL;

	g->smod_entry = a.va;

	/*
	 * The user RSP for the return.  On both the direct switch and the
	 * emulated path, user mode resumes on the RSP supervisor mode had,
	 * so this is also where the user stack gets established.
	 */
	emit_movabs(&a, GPR_RSP, UVA_STACK_TOP);
	/*
	 * The PVCS through the control page too, so that the VMM can move it
	 * between two exits -- pvm/pvcs-alias-rebind does.
	 */
	emit_movabs(&a, GPR_RSI, ctrl);
	EMIT(&a, 0x48, 0x8b, 0x7e, CTRL_PVCS);		/* mov CTRL_PVCS(%rsi),%rdi */

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

/* PVM_HC_LOAD_PGTBL(flags 0, pgd from the control page at @ctrl_off). */
static void emit_load_pgtbl(struct asmbuf *a, uint64_t ctrl, uint8_t ctrl_off)
{
	emit_movabs(a, GPR_RSI, ctrl);
	emit_movabs(a, GPR_RAX, PVM_HC_LOAD_PGTBL);
	EMIT(a, 0x31, 0xdb);				/* xor %ebx,%ebx: flags */
	EMIT(a, 0x4c, 0x8b, 0x56, ctrl_off);		/* mov off(%rsi),%r10: pgd */
	EMIT(a, 0x0f, 0x05);				/* syscall */
}

/*
 * The same supervisor mode, but each round it first loads two page tables
 * with PVM_HC_LOAD_PGTBL: CTRL_PGD1 and then CTRL_PGD2.  Flags 0 -- keep the
 * TLB, 4-level -- is the exact word the switcher is allowed to serve itself.
 * The rest is build_smod_code(): copy the control page into the PVCS and
 * return to user mode, which runs on whatever CTRL_PGD2 names.
 */
static void build_pgtbl_smod_code(struct guest *g)
{
	struct asmbuf a = { g->mem + O_SCODE + 0x200, g->kva + O_SCODE + 0x200 };
	uint64_t pvcs = g->kva + O_PVCS;
	uint64_t ctrl = g->kva + O_CTRL;

	g->smod_entry = a.va;

	emit_load_pgtbl(&a, ctrl, CTRL_PGD1);
	emit_load_pgtbl(&a, ctrl, CTRL_PGD2);

	emit_movabs(&a, GPR_RSP, UVA_STACK_TOP);
	emit_movabs(&a, GPR_RDI, pvcs);
	emit_movabs(&a, GPR_RSI, ctrl);

	EMIT(&a, 0x48, 0x8b, 0x46, CTRL_RIP);		/* mov CTRL_RIP(%rsi),%rax */
	EMIT(&a, 0x48, 0x89, 0x47, PVCS_RIP);		/* mov %rax,PVCS_RIP(%rdi) */
	EMIT(&a, 0x8b, 0x46, CTRL_EFLAGS);		/* mov CTRL_EFLAGS(%rsi),%eax */
	EMIT(&a, 0x89, 0x47, PVCS_EFLAGS);		/* mov %eax,PVCS_EFLAGS(%rdi) */
	EMIT(&a, 0x8b, 0x46, CTRL_SEL);			/* mov CTRL_SEL(%rsi),%eax */
	EMIT(&a, 0x89, 0x47, PVCS_USER_CS);		/* mov %eax,PVCS_USER_CS(%rdi) */

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
 * A second user-mode routine, at UVA_CODE + UCODE_FAULT: read a byte from
 * FAULT_VA, which has no guest PTE.  The #PF goes to the user event handler,
 * which reports it and spins.
 */
#define UCODE_FAULT	0x100
#define FAULT_VA	(UVA_BASE + 0x3000)

static void build_umod_fault_code(struct guest *g)
{
	struct asmbuf a = { g->mem + O_UCODE + UCODE_FAULT, UVA_CODE + UCODE_FAULT };

	emit_movabs(&a, GPR_RDX, FAULT_VA + 0x123);
	EMIT(&a, 0x8a, 0x02);				/* mov (%rdx),%al */
	EMIT(&a, 0x0f, 0x0b);				/* ud2 */
}

/*
 * A third, at UVA_CODE + UCODE_PATTERN, for the repeat rule: each time it is
 * entered it takes the next address from a list on the user stack page and
 * reads it; when the list is used up it reports the count.  With the event
 * entry pointed at the supervisor code, every #PF comes straight back here,
 * so the faults happen in exactly the order of the list and nothing ever
 * maps the addresses.
 */
#define UCODE_PATTERN	0x200
#define PATTERN_OFF	0x100		/* on the user stack page: count, then VAs */
#define PATTERN_UVA	(UVA_BASE + 0x1000 + PATTERN_OFF)
#define PATTERN_MAX	12		/* the total at +104 stays a disp8 */

static void build_umod_pattern_code(struct guest *g)
{
	struct asmbuf a = { g->mem + O_UCODE + UCODE_PATTERN, UVA_CODE + UCODE_PATTERN };
	uint8_t cmp[4] = { 0x48, 0x3b, 0x4e, 0x00 };

	emit_movabs(&a, GPR_RSI, PATTERN_UVA);
	EMIT(&a, 0x48, 0x8b, 0x0e);			/* mov (%rsi),%rcx: done */
	cmp[3] = PATTERN_MAX * 8 + 8;
	emit(&a, cmp, sizeof(cmp));			/* cmp total(%rsi),%rcx */
	EMIT(&a, 0x73, 0x0c);				/* jae report */
	EMIT(&a, 0x48, 0xff, 0x06);			/* incq (%rsi) */
	EMIT(&a, 0x48, 0x8b, 0x54, 0xce, 0x08);		/* mov 8(%rsi,%rcx,8),%rdx */
	EMIT(&a, 0x8a, 0x02);				/* mov (%rdx),%al */
	EMIT(&a, 0x0f, 0x0b);				/* ud2 */
	/* report: */
	emit_movabs(&a, GPR_RDX, UVA_MMIO);
	EMIT(&a, 0x89, 0x0a);				/* mov %ecx,(%rdx) */
	EMIT(&a, 0x0f, 0x05);				/* syscall */
	EMIT(&a, 0x0f, 0x0b);				/* ud2 */
}

/*
 * A supervisor-mode probe, entered only by a case that points RIP at it:
 * PVM_CPUID_FEATURES through the synthetic CPUID, then CR2_PROBE written
 * into PVCS::cr2 -- the guest's way of setting CR2 -- and ebx reported on
 * the event MMIO page.
 */
#define CR2_PROBE	0x00007f0012345678ULL

static uint64_t build_smod_probe_code(struct guest *g)
{
	struct asmbuf a = { g->mem + O_SCODE + 0x600, g->kva + O_SCODE + 0x600 };
	uint64_t entry = a.va;
	uint8_t mov_eax[5] = { 0xb8 };
	uint32_t leaf = PVM_CPUID_FEATURES;

	memcpy(mov_eax + 1, &leaf, 4);
	emit(&a, mov_eax, sizeof(mov_eax));		/* mov $leaf,%eax */
	EMIT(&a, 0x31, 0xc9);				/* xor %ecx,%ecx */
	EMIT(&a, PVM_SYNTHETIC_CPUID);
	emit_movabs(&a, GPR_RDI, g->kva + O_PVCS);
	emit_movabs(&a, GPR_RAX, CR2_PROBE);
	EMIT(&a, 0x48, 0x89, 0x47, PVCS_CR2);		/* mov %rax,cr2(%rdi) */
	emit_movabs(&a, GPR_RDX, g->kva + 0x10000);	/* the event MMIO page */
	EMIT(&a, 0x89, 0x1a);				/* mov %ebx,(%rdx) */
	EMIT(&a, 0xeb, 0xfe);				/* 1: jmp 1b */
	return entry;
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

	/* The page pvm/pvcs-alias-rebind moves the PVCS to. */
	put_pte(g, O_PT_K, PT_INDEX(g->kva + O_PVCS2, 1), (base + O_PVCS2) | PTE_P | PTE_RW);

	/* kva + 0x10000: the MMIO page the event handlers write. */
	put_pte(g, O_PT_K, PT_INDEX(g->kva + 0x10000, 1), MMIO_EVENT | PTE_P | PTE_RW);

	put_pte(g, O_PML4, PT_INDEX(UVA_BASE, 4), (base + O_PDP_U) | PTE_P | PTE_RW | PTE_US);
	put_pte(g, O_PDP_U, PT_INDEX(UVA_BASE, 3), (base + O_PD_U) | PTE_P | PTE_RW | PTE_US);
	put_pte(g, O_PD_U, PT_INDEX(UVA_BASE, 2), (base + O_PT_U) | PTE_P | PTE_RW | PTE_US);

	put_pte(g, O_PT_U, PT_INDEX(UVA_CODE, 1), (base + O_UCODE) | PTE_P | PTE_RW | PTE_US);
	put_pte(g, O_PT_U, PT_INDEX(UVA_BASE + 0x1000, 1),
		(base + O_USTACK) | PTE_P | PTE_RW | PTE_US);
	put_pte(g, O_PT_U, PT_INDEX(UVA_MMIO, 1), MMIO_REPORT | PTE_P | PTE_RW | PTE_US);

	/*
	 * The other address space shares the kernel half and has no user
	 * half at all.  Supervisor mode can run on it; user mode never does,
	 * so the hypervisor never has a user-side root to pair with it.
	 */
	put_pte(g, O_PML4_ALT, PT_INDEX(g->kva, 4), (base + O_PDP_K) | PTE_P | PTE_RW);
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
	/*
	 * PCIDE as well, as a Linux guest has.  PVM_HC_LOAD_PGTBL without the
	 * TLB flag turns into a CR3 load with NOFLUSH, and without PCIDE that
	 * is a reserved bit: kvm_set_cr3() refuses it and the hypercall does
	 * nothing at all, which is how the pgtbl case first "passed" on a
	 * kernel that had the bug it checks for.
	 */
	s.cr4 = (1UL << 5) | (1UL << 17);	/* PAE | PCIDE */
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
	if (g->pgtbl_variant)
		build_pgtbl_smod_code(g);
	build_umod_code(g);
	build_umod_fault_code(g);
	build_umod_pattern_code(g);
	build_event_code(g, 0, MARK_USER_EVENT);
	build_event_code(g, 512, MARK_SUPERVISOR_EVENT);
	build_page_tables(g);
	{
		uint64_t pvcs_kva = g->kva + O_PVCS;

		memcpy(g->mem + O_CTRL + CTRL_PVCS, &pvcs_kva, 8);
	}

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

static void ctrl_set_pgds(struct guest *g, uint64_t pgd1, uint64_t pgd2)
{
	uint8_t *c = g->mem + O_CTRL;

	memcpy(c + CTRL_PGD1, &pgd1, 8);
	memcpy(c + CTRL_PGD2, &pgd2, 8);
}

/*
 * The vCPU's "exits" counter from its binary stats file: every return from
 * guest mode to the hypervisor, including the ones userspace never sees.  A
 * direct switch is not one; an emulated ERETU is.  That is the only way from
 * here to tell the two paths apart by count rather than by outcome.
 */
static int vcpu_stat(struct vm *v, const char *want, uint64_t *out)
{
	struct kvm_stats_header h;
	struct kvm_stats_desc *d;
	size_t dsz;
	uint32_t i;
	int fd, r = -1;

	fd = ioctl(v->vcpu, KVM_GET_STATS_FD, NULL);
	if (fd < 0)
		return -1;
	if (pread(fd, &h, sizeof(h), 0) != sizeof(h))
		goto out;

	dsz = sizeof(*d) + h.name_size;
	d = malloc(dsz);
	if (!d)
		goto out;
	for (i = 0; i < h.num_desc; i++) {
		if (pread(fd, d, dsz, h.desc_offset + i * dsz) != (ssize_t)dsz)
			break;
		if (strcmp(d->name, want))
			continue;
		if (pread(fd, out, sizeof(*out), h.data_offset + d->offset) == sizeof(*out))
			r = 0;
		break;
	}
	free(d);
out:
	close(fd);
	return r;
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

/*
 * SWITCH_FLAGS_NO_DS_CR3 has to follow the address space the switcher loads,
 * in both directions.
 *
 * Each round supervisor mode loads CTRL_PGD1 and then CTRL_PGD2 and returns to
 * user mode on the second.  User mode's SYSCALL back is a direct switch, so
 * the first load always finds a table the hypervisor built in user mode --
 * empty -- and exits.  The hypervisor serves it and re-enters in supervisor
 * mode on PGD1 with a fresh table, and the second load and the return to user
 * are then the switcher's to serve.
 *
 * With PGD1 the alternate address space, which never runs in user mode and so
 * has no user-side root, that re-entry sets NO_DS_CR3.  Loading PGD2 through
 * the switcher must clear it again, since PGD2's pair is cached; a switcher
 * that only ever sets it turns the return to user into one more exit per
 * round.  The control run loads the main address space twice and has no
 * reason to set the bit at all.  Both runs are the same code with the same
 * number of guest instructions, so the difference between them is the bug.
 */
#define PGTBL_ROUNDS 200

static int pgtbl_round_exits(struct vm *v, uint64_t *exits)
{
	struct outcome o;
	uint64_t before, after;
	char desc[256];

	if (vcpu_stat(v, "exits", &before)) {
		nok("could not read the vCPU's exits counter: %s", strerror(errno));
		return -1;
	}
	guest_run(v, &o, PGTBL_ROUNDS, 30);
	if (vcpu_stat(v, "exits", &after)) {
		nok("could not read the vCPU's exits counter: %s", strerror(errno));
		return -1;
	}
	if (o.reports < PGTBL_ROUNDS) {
		describe(&o, desc, sizeof(desc));
		nok("the guest stopped going round: %s", desc);
		return -1;
	}
	*exits = after - before;
	return 0;
}

static void test_pgtbl_no_ds_cr3(struct vm *v, struct guest *g)
{
	uint64_t main_as = GUEST_PHYS_BASE + O_PML4;
	uint64_t alt_as = GUEST_PHYS_BASE + O_PML4_ALT;
	uint64_t control, stale;
	double extra;

	current_case = "pvm/pgtbl-fastpath-clears-no-ds-cr3";

	ctrl_set_pgds(g, main_as, main_as);
	if (warm_up(v, g, "pgtbl"))
		return;

	if (pgtbl_round_exits(v, &control))
		return;

	ctrl_set_pgds(g, alt_as, main_as);
	/* One round to settle the alternate root into the MMU's cache. */
	if (warm_up(v, g, "pgtbl"))
		return;
	if (pgtbl_round_exits(v, &stale))
		return;

	extra = ((double)stale - (double)control) / PGTBL_ROUNDS;
	if (extra >= 0.5)
		nok("%.2f more exits per round through an address space without "
		    "a pair (%llu vs %llu over %d rounds): returns to user mode "
		    "are exiting after the switcher loaded a paired root",
		    extra, (unsigned long long)stale,
		    (unsigned long long)control, PGTBL_ROUNDS);
	else
		ok("%llu exits with the unpaired detour, %llu without, over %d rounds",
		   (unsigned long long)stale, (unsigned long long)control,
		   PGTBL_ROUNDS);
}

/* --- the PVCS alias (host KPTI) and its lifecycle ----------------------- */

/*
 * Rounds of well-formed direct switches, every one of which must come back as
 * a report with sane RFLAGS and none as an event.  Returns the vCPU exits the
 * rounds took, or -1 having said why.
 */
static long run_clean_rounds(struct vm *v, int rounds, int seconds)
{
	struct outcome o;
	uint64_t before, after;
	char desc[256];

	if (vcpu_stat(v, "exits", &before))
		before = 0;
	guest_run(v, &o, rounds, seconds);
	if (vcpu_stat(v, "exits", &after))
		after = 0;
	describe(&o, desc, sizeof(desc));
	if (o.reports < rounds || o.events) {
		nok("%s", desc);
		return -1;
	}
	if (!(o.last_rflags & 0x200)) {
		nok("user mode got RFLAGS %#x: %s", o.last_rflags, desc);
		return -1;
	}
	return (long)(after - before);
}

/*
 * The PVCS moves to another page while the guest runs.
 *
 * The VMM copies it, points MSR_PVM_VCPU_STRUCT and the guest at the new page,
 * and poisons the old one: a return RIP of an unmapped address and an ERETU
 * target the guest never uses.  Every direct switch after that reads and
 * writes the PVCS through tss_ex.pvcs -- the per-VM alias under host KPTI --
 * so a translation still pointing at the old page sends user mode to the
 * poison and the round comes back as an event, not a report.  That is the
 * "no stale ASID keeps the old mapping" invariant, observed.
 *
 * The rounds are also counted: they have to stay direct switches, one exit
 * each for the report, or the case would pass by never touching the alias.
 */
#define REBIND_ROUNDS 200

static void test_pvcs_alias_rebind(struct vm *v, struct guest *g)
{
	uint64_t kva2 = g->kva + O_PVCS2, poison = 0x0000dead00000000ULL;
	long exits;
	int i;

	current_case = "pvm/pvcs-alias-rebind";
	if (warm_up(v, g, "rebind"))
		return;

	for (i = 0; i < 3; i++) {
		uint64_t from = i % 2 ? O_PVCS2 : O_PVCS;
		uint64_t to = i % 2 ? O_PVCS : O_PVCS2;
		uint64_t to_kva = i % 2 ? g->kva + O_PVCS : kva2;

		memcpy(g->mem + to, g->mem + from, 0x1000);
		memcpy(g->mem + O_CTRL + CTRL_PVCS, &to_kva, 8);
		if (set_msr(v, MSR_PVM_VCPU_STRUCT, GUEST_PHYS_BASE + to) <= 0) {
			nok("moving the PVCS to %#lx was refused", GUEST_PHYS_BASE + to);
			return;
		}
		memcpy(g->mem + from + PVCS_RIP, &poison, 8);

		exits = run_clean_rounds(v, REBIND_ROUNDS, 20);
		if (exits < 0)
			return;
		if (exits > 2 * REBIND_ROUNDS) {
			nok("move %d: %ld exits over %d rounds -- not direct switches",
			    i + 1, exits, REBIND_ROUNDS);
			return;
		}
	}
	ok("PVCS moved 3 times, %d clean direct-switch rounds after each", REBIND_ROUNDS);
}

/*
 * Direct switches while the vCPU thread is moved across every CPU the process
 * may run on, every millisecond.  The alias is per vCPU and needs no remapping
 * when the vCPU changes CPU, but the ASID does change, and a PCID left over on
 * the CPU it came back to is exactly the stale-translation case to rule out.
 */
struct migrator {
	pid_t tid;
	volatile int stop;
	unsigned long moves;
};

static void *migrate_thread(void *arg)
{
	struct migrator *m = arg;
	cpu_set_t allowed, one;
	int cpu = 0;

	if (sched_getaffinity(m->tid, sizeof(allowed), &allowed))
		return NULL;
	while (!m->stop) {
		do {
			cpu = (cpu + 1) % CPU_SETSIZE;
		} while (!CPU_ISSET(cpu, &allowed));
		CPU_ZERO(&one);
		CPU_SET(cpu, &one);
		if (!sched_setaffinity(m->tid, sizeof(one), &one))
			m->moves++;
		usleep(1000);
	}
	sched_setaffinity(m->tid, sizeof(allowed), &allowed);
	return NULL;
}

#define MIGRATE_ROUNDS 20000

static void test_pvcs_alias_migrate(struct vm *v, struct guest *g)
{
	struct migrator m = { .tid = gettid() };
	pthread_t th;
	long exits;

	current_case = "pvm/pvcs-alias-migrate";
	if (warm_up(v, g, "migrate"))
		return;
	if (pthread_create(&th, NULL, migrate_thread, &m)) {
		nok("pthread_create: %s", strerror(errno));
		return;
	}
	exits = run_clean_rounds(v, MIGRATE_ROUNDS, 120);
	m.stop = 1;
	pthread_join(th, NULL);
	if (exits < 0)
		return;
	if (m.moves < 10) {
		nok("only %lu CPU moves happened; nothing was tested", m.moves);
		return;
	}
	ok("%d rounds, %ld exits, across %lu CPU moves", MIGRATE_ROUNDS, exits, m.moves);
}

/*
 * The alias is PVM_PVCS_ALIAS_BASE + vcpu_idx pages.  Create every vCPU the VM
 * may have, give each a PVCS page filled with a pattern, and run only the
 * last one: its direct switches write through the highest alias there is.
 * Every other vCPU's page must still hold its pattern afterwards, and one vCPU
 * more than the maximum must be refused.
 */
#define DUMMY_PVCS_BASE	0x100000	/* offset into guest memory: 1MB up */
#define PATTERN(i)	(0x5a00000000000000ULL | (uint64_t)(i))

static void test_pvcs_alias_last_vcpu(void)
{
	struct vm v = {};
	struct guest g = {};
	int max, i, *fds, extra, bad = -1;
	long exits;

	current_case = "pvm/pvcs-alias-last-vcpu";
	vm_setup(&v);
	max = ioctl(v.kvm, KVM_CHECK_EXTENSION, KVM_CAP_MAX_VCPUS);
	if (max <= 1 || DUMMY_PVCS_BASE + (uint64_t)max * 0x1000 > GUEST_MEM_SIZE) {
		nok("KVM_CAP_MAX_VCPUS is %d, which this case cannot lay out", max);
		vm_teardown(&v);
		return;
	}

	{
		struct rlimit rl;

		getrlimit(RLIMIT_NOFILE, &rl);
		rl.rlim_cur = rl.rlim_max;
		setrlimit(RLIMIT_NOFILE, &rl);
	}

	fds = calloc(max, sizeof(*fds));
	/* vCPU 0 is v.vcpu; 1 .. max-2 are the dummies. */
	for (i = 1; i < max - 1; i++) {
		fds[i] = ioctl(v.vm, KVM_CREATE_VCPU, i);
		if (fds[i] < 0) {
			nok("creating vCPU %d of %d: %s", i, max, strerror(errno));
			goto out;
		}
	}
	/* The last one becomes the vCPU that runs. */
	fds[max - 1] = ioctl(v.vm, KVM_CREATE_VCPU, max - 1);
	if (fds[max - 1] < 0) {
		nok("creating the last vCPU (%d): %s", max - 1, strerror(errno));
		goto out;
	}
	extra = ioctl(v.vm, KVM_CREATE_VCPU, max);
	if (extra >= 0) {
		nok("vCPU %d of a maximum of %d was created", max + 1, max);
		close(extra);
		goto out;
	}

	munmap(v.run, v.run_size);
	fds[0] = v.vcpu;
	v.vcpu = fds[max - 1];
	v.run = mmap(NULL, v.run_size, PROT_READ | PROT_WRITE, MAP_SHARED, v.vcpu, 0);
	if (v.run == MAP_FAILED || ioctl(v.vcpu, KVM_SET_CPUID2, v.cpuid) < 0) {
		nok("setting up the last vCPU: %s", strerror(errno));
		v.vcpu = fds[0];
		goto out;
	}

	if (guest_setup(&v, &g)) {
		nok("could not build the guest on vCPU %d", max - 1);
		goto swap_back;
	}

	for (i = 0; i < max - 1; i++) {
		int fd = i ? fds[i] : fds[0];
		struct vm dv = v;
		uint64_t off = DUMMY_PVCS_BASE + (uint64_t)i * 0x1000, pat = PATTERN(i);
		int j;

		for (j = 0; j < 0x1000; j += 8)
			memcpy((uint8_t *)v.mem + off + j, &pat, 8);
		dv.vcpu = fd;
		if (set_msr(&dv, MSR_PVM_VCPU_STRUCT, GUEST_PHYS_BASE + off) <= 0) {
			nok("pinning a PVCS for vCPU %d was refused", i);
			goto swap_back;
		}
	}

	if (warm_up(&v, &g, "last-vcpu"))
		goto swap_back;
	exits = run_clean_rounds(&v, 200, 20);
	if (exits < 0)
		goto swap_back;

	for (i = 0; i < max - 1 && bad < 0; i++) {
		uint64_t off = DUMMY_PVCS_BASE + (uint64_t)i * 0x1000, pat = PATTERN(i), got;
		int j;

		for (j = 0; j < 0x1000; j += 8) {
			memcpy(&got, (uint8_t *)v.mem + off + j, 8);
			if (got != pat) {
				bad = i;
				break;
			}
		}
	}
	if (bad >= 0)
		nok("vCPU %d's PVCS page was written while only vCPU %d ran", bad, max - 1);
	else if (exits > 400)
		nok("%ld exits over 200 rounds on vCPU %d -- not direct switches", exits, max - 1);
	else
		ok("%d vCPUs; vCPU %d ran 200 direct-switch rounds (%ld exits), "
		   "the other %d PVCS pages untouched, vCPU %d refused",
		   max, max - 1, exits, max - 1, max + 1);

swap_back:
	munmap(v.run, v.run_size);
	v.run = mmap(NULL, v.run_size, PROT_READ | PROT_WRITE, MAP_SHARED, fds[0], 0);
	v.vcpu = fds[0];
	fds[max - 1] = fds[max - 1] >= 0 ? fds[max - 1] : -1;
out:
	for (i = 1; i < max; i++)
		if (fds[i] > 0)
			close(fds[i]);
	free(fds);
	vm_teardown(&v);
}


/* --- PVM_FEATURE_DIRECT_PF ---------------------------------------------- */

/*
 * The guest's view of the feature: the synthetic CPUID it would check, and
 * CR2 as KVM reports it after the guest sets it the PVM way, through
 * PVCS::cr2, and exits.
 */
static void test_direct_pf_cpuid_and_cr2_write(struct vm *v, struct guest *g)
{
	struct kvm_regs r;
	struct kvm_sregs s;
	struct outcome o;
	char desc[256];

	current_case = "pvm/direct-pf/cpuid-and-cr2-write";
	if (ioctl(v->vcpu, KVM_GET_REGS, &r) < 0) {
		nok("KVM_GET_REGS: %s", strerror(errno));
		return;
	}
	r.rip = build_smod_probe_code(g);
	if (ioctl(v->vcpu, KVM_SET_REGS, &r) < 0) {
		nok("KVM_SET_REGS: %s", strerror(errno));
		return;
	}

	guest_run(v, &o, 1, 10);
	describe(&o, desc, sizeof(desc));
	if (o.events != 1) {
		nok("the probe never reported: %s", desc);
		return;
	}
	if (!(o.last_event & PVM_FEATURE_DIRECT_PF)) {
		nok("PVM_CPUID_FEATURES.ebx is %#x, without PVM_FEATURE_DIRECT_PF",
		    o.last_event);
		return;
	}
	if (ioctl(v->vcpu, KVM_GET_SREGS, &s) < 0) {
		nok("KVM_GET_SREGS: %s", strerror(errno));
		return;
	}
	if (s.cr2 != CR2_PROBE)
		nok("guest wrote %#llx to PVCS::cr2, KVM reports CR2 %#llx",
		    (unsigned long long)CR2_PROBE, (unsigned long long)s.cr2);
	else
		ok("ebx %#x; CR2 %#llx after the guest wrote it", o.last_event,
		   (unsigned long long)s.cr2);
}

/*
 * A user read of an address with no guest PTE, with the feature enabled
 * (@direct) or not.  Either way the guest must see the same fault -- vector
 * 14 at the user entry, error code U, PVCS::cr2 the address -- and KVM must
 * report that CR2 afterwards.  What differs is who delivered it: pf_guest
 * counts the #PFs KVM injected, which a switcher delivery never is.  (Not
 * pf_taken: the event handler's own first faults, in supervisor mode, count
 * there.)
 */
static void test_direct_pf_fault(struct vm *v, struct guest *g, bool direct)
{
	uint64_t want_va = FAULT_VA + 0x123, taken0 = 0, taken1 = 0, pvcs_cr2;
	uint16_t errcode;
	struct kvm_sregs s;
	struct outcome o;
	char desc[256];

	current_case = direct ? "pvm/direct-pf/user-np-delivered-directly" :
				"pvm/direct-pf/user-np-disabled-slow-path";
	/*
	 * Warm up first, with the feature off: the warm-up's own first
	 * touches of present pages would otherwise be delivered spuriously,
	 * to an event handler that only spins.
	 */
	if (warm_up(v, g, current_case))
		return;
	if (set_msr(v, MSR_PVM_FEATURES_ENABLED, direct ? PVM_FEATURE_DIRECT_PF : 0) <= 0) {
		nok("MSR_PVM_FEATURES_ENABLED write refused");
		return;
	}
	if (vcpu_stat(v, "pf_guest", &taken0)) {
		nok("no pf_guest stat");
		return;
	}

	ctrl_set(g, UVA_CODE + UCODE_FAULT, 0x202, GOOD_SEL);
	guest_run(v, &o, 1, 10);
	describe(&o, desc, sizeof(desc));
	vcpu_stat(v, "pf_guest", &taken1);

	if (o.events != 1 || o.last_event != (MARK_USER_EVENT | 0x100 | 14)) {
		nok("expected a #PF at the user event entry: %s", desc);
		return;
	}
	memcpy(&errcode, g->mem + O_PVCS + PVCS_ERRCODE, 2);
	memcpy(&pvcs_cr2, g->mem + O_PVCS + PVCS_CR2, 8);
	if (ioctl(v->vcpu, KVM_GET_SREGS, &s) < 0) {
		nok("KVM_GET_SREGS: %s", strerror(errno));
		return;
	}
	if (errcode != 0x4)
		nok("error code %#x, want U (0x4)", errcode);
	else if (pvcs_cr2 != want_va)
		nok("PVCS::cr2 %#llx, want %#llx", (unsigned long long)pvcs_cr2,
		    (unsigned long long)want_va);
	else if (s.cr2 != want_va)
		nok("KVM reports CR2 %#llx after the fault, want %#llx",
		    (unsigned long long)s.cr2, (unsigned long long)want_va);
	else if (direct && taken1 != taken0)
		nok("KVM injected the fault (pf_guest %llu -> %llu): "
		    "not delivered by the switcher", (unsigned long long)taken0,
		    (unsigned long long)taken1);
	else if (!direct && taken1 == taken0)
		nok("KVM never injected the fault with the feature off");
	else
		ok("#PF U at %#llx, CR2 synced, pf_guest +%llu", (unsigned long long)want_va,
		   (unsigned long long)(taken1 - taken0));
}

/*
 * The repeat rule, deterministically.  The guest faults on @n addresses in
 * the order given, none of them mapped, and resumes after each; pf_taken
 * says how many reached the shadow MMU instead of being delivered directly.
 * The rule fixes that number: never twice in a row on a page, and never more
 * than four deliveries in a row.
 */
static void test_direct_pf_rule(struct vm *v, struct guest *g, const char *name,
				const char *pattern, uint64_t want_slow)
{
	uint64_t n = strlen(pattern), total = n, zero = 0, taken0 = 0, taken1 = 0, i;
	uint8_t *p = g->mem + O_USTACK + PATTERN_OFF;
	struct outcome o;
	char desc[256];

	current_case = name;
	if (warm_up(v, g, name))
		return;
	if (set_msr(v, MSR_PVM_FEATURES_ENABLED, PVM_FEATURE_DIRECT_PF) <= 0) {
		nok("MSR_PVM_FEATURES_ENABLED write refused");
		return;
	}

	/* Every #PF back to user mode at the pattern routine, via the smod code. */
	if (set_msr(v, MSR_PVM_EVENT_ENTRY, g->smod_entry) <= 0) {
		nok("MSR_PVM_EVENT_ENTRY write refused");
		return;
	}
	memcpy(p, &zero, 8);
	for (i = 0; i < n; i++) {
		uint64_t va = UVA_BASE + 0x10000 + (uint64_t)(pattern[i] - 'A') * 0x1000 + 0x40;

		memcpy(p + 8 + i * 8, &va, 8);
	}
	memcpy(p + 8 + PATTERN_MAX * 8, &total, 8);
	ctrl_set(g, UVA_CODE + UCODE_PATTERN, 0x202, GOOD_SEL);

	if (vcpu_stat(v, "pf_taken", &taken0)) {
		nok("no pf_taken stat");
		return;
	}
	guest_run(v, &o, 1, 10);
	vcpu_stat(v, "pf_taken", &taken1);
	describe(&o, desc, sizeof(desc));

	if (o.reports != 1 || o.last_rflags != n)
		nok("the pattern did not run to the end: %s", desc);
	else if (taken1 - taken0 != want_slow)
		nok("pattern %s: %llu of %llu faults reached the shadow MMU, the "
		    "rule says %llu", pattern, (unsigned long long)(taken1 - taken0),
		    (unsigned long long)n, (unsigned long long)want_slow);
	else
		ok("pattern %s: %llu direct, %llu through the MMU", pattern,
		   (unsigned long long)(n - want_slow), (unsigned long long)want_slow);
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

	vm_setup(&v);
	g.pgtbl_variant = true;
	if (!guest_setup(&v, &g))
		test_pgtbl_no_ds_cr3(&v, &g);
	g.pgtbl_variant = false;
	vm_teardown(&v);

	vm_setup(&v);
	if (!guest_setup(&v, &g))
		test_pvcs_alias_rebind(&v, &g);
	vm_teardown(&v);

	vm_setup(&v);
	if (!guest_setup(&v, &g))
		test_pvcs_alias_migrate(&v, &g);
	vm_teardown(&v);

	test_pvcs_alias_last_vcpu();

	vm_setup(&v);
	if (!guest_setup(&v, &g))
		test_direct_pf_cpuid_and_cr2_write(&v, &g);
	vm_teardown(&v);

	vm_setup(&v);
	if (!guest_setup(&v, &g))
		test_direct_pf_fault(&v, &g, true);
	vm_teardown(&v);

	vm_setup(&v);
	if (!guest_setup(&v, &g))
		test_direct_pf_fault(&v, &g, false);
	vm_teardown(&v);

	{
		/*
		 * AAAAAA: every other one is the same page as the delivery
		 * before it.  AABCDEFG: the second A resets the run, then four
		 * deliveries, F refused, G delivered.  AABCBCBC: B and C
		 * alternate, so only the run limit refuses one.
		 */
		static const struct {
			const char *name, *pattern;
			uint64_t slow;
		} rules[] = {
			{ "pvm/direct-pf/rule-same-page", "AAAAAA", 3 },
			{ "pvm/direct-pf/rule-run-limit", "AABCDEFG", 2 },
			{ "pvm/direct-pf/rule-interleaved", "AABCBCBC", 2 },
		};
		size_t i;

		for (i = 0; i < sizeof(rules) / sizeof(rules[0]); i++) {
			vm_setup(&v);
			if (!guest_setup(&v, &g))
				test_direct_pf_rule(&v, &g, rules[i].name,
						    rules[i].pattern, rules[i].slow);
			vm_teardown(&v);
		}
	}

	printf("1..%d\n", pass + fail);
	printf("PVMHOSTTEST-RESULT: %s pass=%d fail=%d\n",
	       fail ? "fail" : "ok", pass, fail);
	return fail ? 1 : 0;
}
