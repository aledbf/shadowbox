package main

import (
	"encoding/binary"
	"syscall"
	"unsafe"

	. "github.com/aledbf/pvm-testbed/tools/hosttests/internal/harness"
	"github.com/aledbf/pvm-testbed/tools/hosttests/internal/kvm"
	. "github.com/aledbf/pvm-testbed/tools/hosttests/internal/pvm"
)

// The host's own selectors.  The switcher compares PVCS::user_cs/user_ss
// against this exact pair, because the guest at CPL3 runs on the host's GDT.
// They are ABI on x86_64 and have been since the beginning.
const (
	HOST_USER_CS = 0x33
	HOST_USER_DS = 0x2b
	GOOD_SEL     = uint32(HOST_USER_DS)<<16 | HOST_USER_CS
)

// Guest physical layout, all offsets from GUEST_PHYS_BASE.
const (
	O_PML4         = 0x0000
	O_PDP_K        = 0x1000
	O_PD_K         = 0x2000
	O_PT_K         = 0x3000
	O_PDP_U        = 0x4000
	O_PD_U         = 0x5000
	O_PT_U         = 0x6000
	O_PVCS         = 0x7000
	O_CTRL         = 0x8000
	O_SCODE        = 0x9000
	O_EVENT        = 0xa000
	O_UCODE        = 0xb000
	O_USTACK       = 0xc000
	O_MAPPED_PAGES = 13     // everything above, mapped into the kernel
	O_PML4_ALT     = 0xd000 // a second address space: kernel half only
	O_PVCS2        = 0xe000 // where pvm/pvcs-alias-rebind moves the PVCS
)

// Two pages with no memslot behind them: the guest's only output device.
const (
	MMIO_REPORT = 0xd0000000 // written from user mode
	MMIO_EVENT  = 0xd0001000 // written from the event handlers
)

// User virtual addresses.  Lower half, so always an allowed guest VA.
const (
	UVA_BASE      = 0x40000000
	UVA_CODE      = UVA_BASE + 0x0000
	UVA_STACK_TOP = UVA_BASE + 0x2000
	UVA_MMIO      = UVA_BASE + 0x2000
)

// Offsets into the control page the guest reads its next PVCS values from.
const (
	CTRL_RIP    = 0x00 // u64
	CTRL_EFLAGS = 0x08 // u32
	CTRL_SEL    = 0x0c // u32: user_cs | user_ss << 16
	CTRL_PGD1   = 0x10 // u64: first CR3 the pgtbl variant loads
	CTRL_PGD2   = 0x18 // u64: second, and the one it returns to user on
	CTRL_PVCS   = 0x20 // u64: kernel VA of the PVCS supervisor mode writes
)

const (
	PTE_P  = 1 << 0
	PTE_RW = 1 << 1
	PTE_US = 1 << 2
)

// The markers the event handlers OR the PVCS event vector into.
const (
	MARK_USER_EVENT       = 0xe7000000
	MARK_SUPERVISOR_EVENT = 0xe8000000
)

type guest struct {
	mem          []byte // host mapping of GUEST_PHYS_BASE
	kva          uint64 // guest kernel VA of GUEST_PHYS_BASE
	smodEntry    uint64 // also the LSTAR entry
	retuRIP      uint64 // the SYSCALL that returns to user mode
	eventEntry   uint64
	pgtblVariant bool // supervisor mode loads two CR3s each round
}

// --- hand assembly -----------------------------------------------------

type asmbuf struct {
	p  []byte // the rest of the page, from the next byte to be emitted
	va uint64 // VA of the next byte to be emitted
}

func (a *asmbuf) emit(b ...byte) {
	n := copy(a.p, b)
	if n != len(b) {
		panic("asmbuf: out of room")
	}
	a.p = a.p[n:]
	a.va += uint64(n)
}

// movabs $imm64, %reg -- REX.W B8+r imm64
func (a *asmbuf) emitMovabs(reg int, imm uint64) {
	b := make([]byte, 10)
	b[0] = 0x48
	b[1] = byte(0xb8 + reg)
	binary.LittleEndian.PutUint64(b[2:], imm)
	a.emit(b...)
}

const (
	GPR_RAX = 0
	GPR_RDX = 2
	GPR_RSP = 4
	GPR_RSI = 6
	GPR_RDI = 7
)

// --- the guest ----------------------------------------------------------

// buildSmodCode is supervisor mode.  Entered twice over: once because
// KVM_SET_REGS points RIP here, and thereafter because it is MSR_LSTAR, so a
// SYSCALL from user mode arrives here too.
//
// It copies three fields from the control page into the PVCS and returns to
// user mode.  That indirection is the whole point: the VMM can plant a
// hostile rip, EFLAGS or selector pair between two guest exits, without the
// guest having to know which case is running, and the switcher's umod->smod
// direction -- which overwrites PVCS::rip with the user return address --
// cannot undo it, because the copy happens after it.
func (g *guest) buildSmodCode() {
	a := asmbuf{g.mem[O_SCODE:], g.kva + O_SCODE}
	ctrl := g.kva + O_CTRL

	g.smodEntry = a.va

	// The user RSP for the return.  On both the direct switch and the
	// emulated path, user mode resumes on the RSP supervisor mode had, so
	// this is also where the user stack gets established.
	a.emitMovabs(GPR_RSP, UVA_STACK_TOP)
	// The PVCS through the control page too, so that the VMM can move it
	// between two exits -- pvm/pvcs-alias-rebind does.
	a.emitMovabs(GPR_RSI, ctrl)
	a.emit(0x48, 0x8b, 0x7e, CTRL_PVCS) // mov CTRL_PVCS(%rsi),%rdi

	a.emit(0x48, 0x8b, 0x46, CTRL_RIP)       // mov CTRL_RIP(%rsi),%rax
	a.emit(0x48, 0x89, 0x47, byte(PVCS_RIP)) // mov %rax,PVCS_RIP(%rdi)
	a.emit(0x8b, 0x46, CTRL_EFLAGS)          // mov CTRL_EFLAGS(%rsi),%eax
	a.emit(0x89, 0x47, byte(PVCS_EFLAGS))    // mov %eax,PVCS_EFLAGS(%rdi)
	a.emit(0x8b, 0x46, CTRL_SEL)             // mov CTRL_SEL(%rsi),%eax
	a.emit(0x89, 0x47, byte(PVCS_USER_CS))   // mov %eax,PVCS_USER_CS(%rdi)

	// The return-to-user synthetic instruction: a SYSCALL at
	// MSR_PVM_RETU_RIP.  The hypervisor adds the 2 itself.
	g.retuRIP = a.va
	a.emit(0x0f, 0x05) // syscall
	a.emit(0x0f, 0x0b) // ud2
}

// emitLoadPgtbl is PVM_HC_LOAD_PGTBL(flags 0, pgd from the control page at
// @ctrlOff).
func (a *asmbuf) emitLoadPgtbl(ctrl uint64, ctrlOff byte) {
	a.emitMovabs(GPR_RSI, ctrl)
	a.emitMovabs(GPR_RAX, PVM_HC_LOAD_PGTBL)
	a.emit(0x31, 0xdb)                // xor %ebx,%ebx: flags
	a.emit(0x4c, 0x8b, 0x56, ctrlOff) // mov off(%rsi),%r10: pgd
	a.emit(0x0f, 0x05)                // syscall
}

// buildPgtblSmodCode is the same supervisor mode, but each round it first
// loads two page tables with PVM_HC_LOAD_PGTBL: CTRL_PGD1 and then
// CTRL_PGD2.  Flags 0 -- keep the TLB, 4-level -- is the exact word the
// switcher is allowed to serve itself.  The rest is buildSmodCode(): copy
// the control page into the PVCS and return to user mode, which runs on
// whatever CTRL_PGD2 names.
func (g *guest) buildPgtblSmodCode() {
	a := asmbuf{g.mem[O_SCODE+0x200:], g.kva + O_SCODE + 0x200}
	pvcs := g.kva + O_PVCS
	ctrl := g.kva + O_CTRL

	g.smodEntry = a.va

	a.emitLoadPgtbl(ctrl, CTRL_PGD1)
	a.emitLoadPgtbl(ctrl, CTRL_PGD2)

	a.emitMovabs(GPR_RSP, UVA_STACK_TOP)
	a.emitMovabs(GPR_RDI, pvcs)
	a.emitMovabs(GPR_RSI, ctrl)

	a.emit(0x48, 0x8b, 0x46, CTRL_RIP)       // mov CTRL_RIP(%rsi),%rax
	a.emit(0x48, 0x89, 0x47, byte(PVCS_RIP)) // mov %rax,PVCS_RIP(%rdi)
	a.emit(0x8b, 0x46, CTRL_EFLAGS)          // mov CTRL_EFLAGS(%rsi),%eax
	a.emit(0x89, 0x47, byte(PVCS_EFLAGS))    // mov %eax,PVCS_EFLAGS(%rdi)
	a.emit(0x8b, 0x46, CTRL_SEL)             // mov CTRL_SEL(%rsi),%eax
	a.emit(0x89, 0x47, byte(PVCS_USER_CS))   // mov %eax,PVCS_USER_CS(%rdi)

	g.retuRIP = a.va
	a.emit(0x0f, 0x05) // syscall
	a.emit(0x0f, 0x0b) // ud2
}

// buildUmodCode is user mode.  Reports the RFLAGS it was given -- which is
// the S2 assertion and doubles as "the transition happened at all" for the
// others -- and syscalls back into supervisor mode.  It never returns from
// that syscall: supervisor mode goes round again and re-enters user mode at
// whatever PVCS::rip the control page now says.
func (g *guest) buildUmodCode() {
	a := asmbuf{g.mem[O_UCODE:], UVA_CODE}

	a.emit(0x9c) // pushfq
	a.emit(0x58) // pop %rax
	a.emitMovabs(GPR_RDX, UVA_MMIO)
	a.emit(0x89, 0x02) // mov %eax,(%rdx)
	a.emit(0x0f, 0x05) // syscall
	a.emit(0x0f, 0x0b) // ud2
}

// A second user-mode routine, at UVA_CODE + UCODE_FAULT: read a byte from
// FAULT_VA, which has no guest PTE.  The #PF goes to the user event handler,
// which reports it and spins.
const (
	UCODE_FAULT = 0x100
	FAULT_VA    = UVA_BASE + 0x3000
)

func (g *guest) buildUmodFaultCode() {
	a := asmbuf{g.mem[O_UCODE+UCODE_FAULT:], UVA_CODE + UCODE_FAULT}

	a.emitMovabs(GPR_RDX, FAULT_VA+0x123)
	a.emit(0x8a, 0x02) // mov (%rdx),%al
	a.emit(0x0f, 0x0b) // ud2
}

// A third, at UVA_CODE + UCODE_PATTERN, for the repeat rule: each time it is
// entered it takes the next address from a list on the user stack page and
// reads it; when the list is used up it reports the count.  With the event
// entry pointed at the supervisor code, every #PF comes straight back here,
// so the faults happen in exactly the order of the list and nothing ever
// maps the addresses.
const (
	UCODE_PATTERN = 0x200
	PATTERN_OFF   = 0x100 // on the user stack page: count, then VAs
	PATTERN_UVA   = UVA_BASE + 0x1000 + PATTERN_OFF
	PATTERN_MAX   = 12 // the total at +104 stays a disp8
)

func (g *guest) buildUmodPatternCode() {
	a := asmbuf{g.mem[O_UCODE+UCODE_PATTERN:], UVA_CODE + UCODE_PATTERN}

	a.emitMovabs(GPR_RSI, PATTERN_UVA)
	a.emit(0x48, 0x8b, 0x0e)                  // mov (%rsi),%rcx: done
	a.emit(0x48, 0x3b, 0x4e, PATTERN_MAX*8+8) // cmp total(%rsi),%rcx
	a.emit(0x73, 0x0c)                        // jae report
	a.emit(0x48, 0xff, 0x06)                  // incq (%rsi)
	a.emit(0x48, 0x8b, 0x54, 0xce, 0x08)      // mov 8(%rsi,%rcx,8),%rdx
	a.emit(0x8a, 0x02)                        // mov (%rdx),%al
	a.emit(0x0f, 0x0b)                        // ud2
	// report:
	a.emitMovabs(GPR_RDX, UVA_MMIO)
	a.emit(0x89, 0x0a) // mov %ecx,(%rdx)
	a.emit(0x0f, 0x05) // syscall
	a.emit(0x0f, 0x0b) // ud2
}

// A supervisor-mode probe, entered only by a case that points RIP at it:
// PVM_CPUID_FEATURES through the synthetic CPUID, then CR2_PROBE written
// into PVCS::cr2 -- the guest's way of setting CR2 -- and ebx reported on
// the event MMIO page.
const CR2_PROBE = 0x00007f0012345678

func (g *guest) buildSmodProbeCode() uint64 {
	a := asmbuf{g.mem[O_SCODE+0x600:], g.kva + O_SCODE + 0x600}
	entry := a.va
	movEAX := []byte{0xb8, 0, 0, 0, 0}

	binary.LittleEndian.PutUint32(movEAX[1:], PVM_CPUID_FEATURES)
	a.emit(movEAX...)  // mov $leaf,%eax
	a.emit(0x31, 0xc9) // xor %ecx,%ecx
	a.emit(PVM_SYNTHETIC_CPUID[:]...)
	a.emitMovabs(GPR_RDI, g.kva+O_PVCS)
	a.emitMovabs(GPR_RAX, CR2_PROBE)
	a.emit(0x48, 0x89, 0x47, byte(PVCS_CR2)) // mov %rax,cr2(%rdi)
	a.emitMovabs(GPR_RDX, g.kva+0x10000)     // the event MMIO page
	a.emit(0x89, 0x1a)                       // mov %ebx,(%rdx)
	a.emit(0xeb, 0xfe)                       // 1: jmp 1b
	return entry
}

// buildEventCode is the event handlers, at MSR_PVM_EVENT_ENTRY and +512 as
// the ABI requires.  Both run in supervisor mode.  Each reports the PVCS
// event vector with a marker saying which one ran, and then spins: the VMM
// stops calling KVM_RUN once it has seen the report, so the spin is never
// executed twice.
func (g *guest) buildEventCode(off uint64, marker uint32) {
	a := asmbuf{g.mem[O_EVENT+off:], g.kva + O_EVENT + off}
	orImm := []byte{0x0d, 0, 0, 0, 0}

	a.emitMovabs(GPR_RDI, g.kva+O_PVCS)
	a.emit(0x0f, 0xb7, 0x47, byte(PVCS_EVENT_VECTOR)) // movzwl vec(%rdi),%eax
	binary.LittleEndian.PutUint32(orImm[1:], marker)
	a.emit(orImm...)                     // or $marker,%eax
	a.emitMovabs(GPR_RDX, g.kva+0x10000) // the event MMIO page
	a.emit(0x89, 0x02)                   // mov %eax,(%rdx)
	a.emit(0xeb, 0xfe)                   // 1: jmp 1b
}

// --- page tables --------------------------------------------------------

func (g *guest) putPTE(table uint64, index uint64, v uint64) {
	binary.LittleEndian.PutUint64(g.mem[table+index*8:], v)
}

func ptIndex(va uint64, level uint) uint64 {
	return (va >> (12 + 9*(level-1))) & 0x1ff
}

// buildPageTables builds one 4-level tree for both of the guest's modes.
// PVM builds two shadow roots from it and tells them apart by the U/S bit,
// which is why the two halves must differ in exactly that: the kernel side
// without _PAGE_USER, the user side with it.
func (g *guest) buildPageTables() {
	const base = GUEST_PHYS_BASE

	g.putPTE(O_PML4, ptIndex(g.kva, 4), (base+O_PDP_K)|PTE_P|PTE_RW)
	g.putPTE(O_PDP_K, ptIndex(g.kva, 3), (base+O_PD_K)|PTE_P|PTE_RW)
	g.putPTE(O_PD_K, ptIndex(g.kva, 2), (base+O_PT_K)|PTE_P|PTE_RW)

	for i := uint64(0); i < O_MAPPED_PAGES; i++ {
		g.putPTE(O_PT_K, ptIndex(g.kva, 1)+i, (base+i*0x1000)|PTE_P|PTE_RW)
	}

	// The page pvm/pvcs-alias-rebind moves the PVCS to.
	g.putPTE(O_PT_K, ptIndex(g.kva+O_PVCS2, 1), (base+O_PVCS2)|PTE_P|PTE_RW)

	// kva + 0x10000: the MMIO page the event handlers write.
	g.putPTE(O_PT_K, ptIndex(g.kva+0x10000, 1), MMIO_EVENT|PTE_P|PTE_RW)

	g.putPTE(O_PML4, ptIndex(UVA_BASE, 4), (base+O_PDP_U)|PTE_P|PTE_RW|PTE_US)
	g.putPTE(O_PDP_U, ptIndex(UVA_BASE, 3), (base+O_PD_U)|PTE_P|PTE_RW|PTE_US)
	g.putPTE(O_PD_U, ptIndex(UVA_BASE, 2), (base+O_PT_U)|PTE_P|PTE_RW|PTE_US)

	g.putPTE(O_PT_U, ptIndex(UVA_CODE, 1), (base+O_UCODE)|PTE_P|PTE_RW|PTE_US)
	g.putPTE(O_PT_U, ptIndex(UVA_BASE+0x1000, 1), (base+O_USTACK)|PTE_P|PTE_RW|PTE_US)
	g.putPTE(O_PT_U, ptIndex(UVA_MMIO, 1), MMIO_REPORT|PTE_P|PTE_RW|PTE_US)

	// The other address space shares the kernel half and has no user half
	// at all.  Supervisor mode can run on it; user mode never does, so the
	// hypervisor never has a user-side root to pair with it.
	g.putPTE(O_PML4_ALT, ptIndex(g.kva, 4), (base+O_PDP_K)|PTE_P|PTE_RW)
}

// --- vCPU state ---------------------------------------------------------

func setSeg(s *kvm.Segment, sel uint16, typ uint8, l, db uint8) {
	*s = kvm.Segment{
		Selector: sel,
		Limit:    0xfffff,
		Type:     typ,
		Present:  1,
		DPL:      0,
		S:        1,
		L:        l,
		DB:       db,
		G:        1,
	}
}

// vcpuEnterLongMode goes straight into 64-bit mode with paging on.  PVM
// comes out of reset in "non-PVM mode", an emulated stand-in for the
// real-mode boot a VMM would otherwise have to do; setting a long-mode CS
// with DPL 0 is what makes try_to_convert_to_pvm_mode() take the vCPU out
// of it.
func vcpuEnterLongMode(v *VM, g *guest) syscall.Errno {
	var s kvm.Sregs
	var r kvm.Regs

	if _, e := Ioctl(v.VCPU, kvm.GET_SREGS, unsafe.Pointer(&s)); e != 0 {
		return e
	}

	s.CR0 = 0x80050033 // PG | AM | WP | NE | ET | MP | PE
	s.CR3 = GUEST_PHYS_BASE + O_PML4
	// PCIDE as well, as a Linux guest has.  PVM_HC_LOAD_PGTBL without the
	// TLB flag turns into a CR3 load with NOFLUSH, and without PCIDE that
	// is a reserved bit: kvm_set_cr3() refuses it and the hypercall does
	// nothing at all, which is how the pgtbl case first "passed" on a
	// kernel that had the bug it checks for.
	s.CR4 = (1 << 5) | (1 << 17) // PAE | PCIDE
	s.EFER = 0xd01               // NXE | LMA | LME | SCE

	setSeg(&s.CS, 0x10, 0xb, 1, 0) // exec/read, accessed, 64-bit
	setSeg(&s.DS, 0x18, 0x3, 0, 1)
	s.ES, s.FS, s.GS, s.SS = s.DS, s.DS, s.DS, s.DS
	s.LDT = kvm.Segment{} // PVM supports no LDT
	s.TR.Present = 1
	s.TR.Type = 11

	if _, e := Ioctl(v.VCPU, kvm.SET_SREGS, unsafe.Pointer(&s)); e != 0 {
		return e
	}

	r.RIP = g.smodEntry
	r.RFLAGS = 0x202
	if _, e := Ioctl(v.VCPU, kvm.SET_REGS, unsafe.Pointer(&r)); e != 0 {
		return e
	}

	return 0
}

// KVA_BASE is where to put the guest kernel.  Any lower-half address will do
// -- that is the whole of what a PVM guest is allowed -- so this is the base
// of the half a real PVM guest kernel uses, picked for documentation value
// rather than necessity.  It used to have to be decoded out of the reset
// value of MSR_PVM_LINEAR_ADDRESS_RANGE, which no longer exists.
const KVA_BASE = 1 << 46

const (
	MSR_LSTAR        = 0xc0000082
	MSR_SYSCALL_MASK = 0xc0000084
)

// buildGuest lays the guest out in @mem: code, page tables and the control
// page.  It is guestSetup() without the vCPU, so the builders can be tested
// without /dev/kvm.
func (g *guest) buildGuest(mem []byte) {
	clear(mem)
	g.mem = mem

	g.kva = KVA_BASE

	g.eventEntry = g.kva + O_EVENT

	g.buildSmodCode()
	if g.pgtblVariant {
		g.buildPgtblSmodCode()
	}
	g.buildUmodCode()
	g.buildUmodFaultCode()
	g.buildUmodPatternCode()
	g.buildEventCode(0, MARK_USER_EVENT)
	g.buildEventCode(512, MARK_SUPERVISOR_EVENT)
	g.buildPageTables()
	binary.LittleEndian.PutUint64(g.mem[O_CTRL+CTRL_PVCS:], g.kva+O_PVCS)
}

func guestSetup(v *VM, g *guest) int {
	g.buildGuest(v.Mem)

	if v.SetMSR(MSR_PVM_VCPU_STRUCT, GUEST_PHYS_BASE+O_PVCS) <= 0 {
		return -1
	}
	if v.SetMSR(MSR_PVM_EVENT_ENTRY, g.eventEntry) <= 0 {
		return -1
	}
	if v.SetMSR(MSR_PVM_RETU_RIP, g.retuRIP) <= 0 {
		return -1
	}
	if v.SetMSR(MSR_LSTAR, g.smodEntry) <= 0 {
		return -1
	}
	if v.SetMSR(MSR_SYSCALL_MASK, 0) <= 0 {
		return -1
	}

	if vcpuEnterLongMode(v, g) != 0 {
		return -1
	}

	return 0
}

func (g *guest) ctrlSet(rip uint64, eflags uint32, sel uint32) {
	c := g.mem[O_CTRL:]

	binary.LittleEndian.PutUint64(c[CTRL_RIP:], rip)
	binary.LittleEndian.PutUint32(c[CTRL_EFLAGS:], eflags)
	binary.LittleEndian.PutUint32(c[CTRL_SEL:], sel)
}

func (g *guest) ctrlSetPgds(pgd1, pgd2 uint64) {
	c := g.mem[O_CTRL:]

	binary.LittleEndian.PutUint64(c[CTRL_PGD1:], pgd1)
	binary.LittleEndian.PutUint64(c[CTRL_PGD2:], pgd2)
}
