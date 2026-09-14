package main

import (
	"bytes"
	"encoding/binary"
	"testing"

	. "github.com/aledbf/pvm-testbed/tools/hosttests/internal/harness"
)

// The expected bytes of each builder are what the C builders in
// the removed hosttests/pvm_switcher_test.c emitted, with KVA_BASE 1<<46, the PVCS offsets
// of struct pvm_vcpu_struct (cr2 8, user_cs 64, event_vector 70, eflags 80,
// rip 88), PVM_HC_LOAD_PGTBL 0x17088200 and PVM_CPUID_FEATURES 0x40000201.

func newTestGuest(t *testing.T, pgtbl bool) *guest {
	t.Helper()
	g := &guest{pgtblVariant: pgtbl}
	g.buildGuest(make([]byte, 0x20000))
	return g
}

func checkBytes(t *testing.T, what string, mem []byte, off int, want []byte) {
	t.Helper()
	got := mem[off : off+len(want)]
	if !bytes.Equal(got, want) {
		t.Errorf("%s:\n got % x\nwant % x", what, got, want)
	}
	// And nothing emitted past the end of it, up to the next routine.
	for i := off + len(want); i < off+len(want)+16 && i < len(mem); i++ {
		if mem[i] != 0 {
			t.Errorf("%s: stray byte %#x at +%#x", what, mem[i], i-off)
			break
		}
	}
}

func TestBuildSmodCode(t *testing.T) {
	g := newTestGuest(t, false)
	want := []byte{
		0x48, 0xbc, 0x00, 0x20, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, // movabs $UVA_STACK_TOP,%rsp
		0x48, 0xbe, 0x00, 0x80, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, // movabs $kva+O_CTRL,%rsi
		0x48, 0x8b, 0x7e, 0x20, // mov CTRL_PVCS(%rsi),%rdi
		0x48, 0x8b, 0x46, 0x00, // mov CTRL_RIP(%rsi),%rax
		0x48, 0x89, 0x47, 0x58, // mov %rax,PVCS_RIP(%rdi)
		0x8b, 0x46, 0x08, // mov CTRL_EFLAGS(%rsi),%eax
		0x89, 0x47, 0x50, // mov %eax,PVCS_EFLAGS(%rdi)
		0x8b, 0x46, 0x0c, // mov CTRL_SEL(%rsi),%eax
		0x89, 0x47, 0x40, // mov %eax,PVCS_USER_CS(%rdi)
		0x0f, 0x05, // syscall
		0x0f, 0x0b, // ud2
	}
	checkBytes(t, "smod", g.mem, O_SCODE, want)
	if g.smodEntry != 0x400000009000 {
		t.Errorf("smod entry %#x, want 0x400000009000", g.smodEntry)
	}
	if g.retuRIP != 0x40000000902c {
		t.Errorf("retu rip %#x, want 0x40000000902c", g.retuRIP)
	}
	if g.eventEntry != 0x40000000a000 {
		t.Errorf("event entry %#x, want 0x40000000a000", g.eventEntry)
	}
}

func TestBuildPgtblSmodCode(t *testing.T) {
	g := newTestGuest(t, true)
	want := []byte{
		// PVM_HC_LOAD_PGTBL(0, CTRL_PGD1)
		0x48, 0xbe, 0x00, 0x80, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, // movabs $ctrl,%rsi
		0x48, 0xb8, 0x00, 0x82, 0x08, 0x17, 0x00, 0x00, 0x00, 0x00, // movabs $PVM_HC_LOAD_PGTBL,%rax
		0x31, 0xdb, // xor %ebx,%ebx
		0x4c, 0x8b, 0x56, 0x10, // mov CTRL_PGD1(%rsi),%r10
		0x0f, 0x05, // syscall
		// PVM_HC_LOAD_PGTBL(0, CTRL_PGD2)
		0x48, 0xbe, 0x00, 0x80, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00,
		0x48, 0xb8, 0x00, 0x82, 0x08, 0x17, 0x00, 0x00, 0x00, 0x00,
		0x31, 0xdb,
		0x4c, 0x8b, 0x56, 0x18, // mov CTRL_PGD2(%rsi),%r10
		0x0f, 0x05,
		0x48, 0xbc, 0x00, 0x20, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, // movabs $UVA_STACK_TOP,%rsp
		0x48, 0xbf, 0x00, 0x70, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, // movabs $kva+O_PVCS,%rdi
		0x48, 0xbe, 0x00, 0x80, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, // movabs $kva+O_CTRL,%rsi
		0x48, 0x8b, 0x46, 0x00, // mov CTRL_RIP(%rsi),%rax
		0x48, 0x89, 0x47, 0x58, // mov %rax,PVCS_RIP(%rdi)
		0x8b, 0x46, 0x08, // mov CTRL_EFLAGS(%rsi),%eax
		0x89, 0x47, 0x50, // mov %eax,PVCS_EFLAGS(%rdi)
		0x8b, 0x46, 0x0c, // mov CTRL_SEL(%rsi),%eax
		0x89, 0x47, 0x40, // mov %eax,PVCS_USER_CS(%rdi)
		0x0f, 0x05, // syscall
		0x0f, 0x0b, // ud2
	}
	checkBytes(t, "pgtbl smod", g.mem, O_SCODE+0x200, want)
	if g.smodEntry != 0x400000009200 {
		t.Errorf("smod entry %#x, want 0x400000009200", g.smodEntry)
	}
	if g.retuRIP != 0x40000000926a {
		t.Errorf("retu rip %#x, want 0x40000000926a", g.retuRIP)
	}
	// The plain supervisor code is still built underneath.
	if g.mem[O_SCODE] != 0x48 || g.mem[O_SCODE+1] != 0xbc {
		t.Errorf("plain smod code missing in the pgtbl variant")
	}
}

func TestBuildUmodCode(t *testing.T) {
	g := newTestGuest(t, false)
	want := []byte{
		0x9c,                                                       // pushfq
		0x58,                                                       // pop %rax
		0x48, 0xba, 0x00, 0x20, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, // movabs $UVA_MMIO,%rdx
		0x89, 0x02, // mov %eax,(%rdx)
		0x0f, 0x05, // syscall
		0x0f, 0x0b, // ud2
	}
	checkBytes(t, "umod", g.mem, O_UCODE, want)
}

func TestBuildUmodFaultCode(t *testing.T) {
	g := newTestGuest(t, false)
	want := []byte{
		0x48, 0xba, 0x23, 0x31, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, // movabs $FAULT_VA+0x123,%rdx
		0x8a, 0x02, // mov (%rdx),%al
		0x0f, 0x0b, // ud2
	}
	checkBytes(t, "umod fault", g.mem, O_UCODE+UCODE_FAULT, want)
}

func TestBuildUmodPatternCode(t *testing.T) {
	g := newTestGuest(t, false)
	want := []byte{
		0x48, 0xbe, 0x00, 0x11, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, // movabs $PATTERN_UVA,%rsi
		0x48, 0x8b, 0x0e, // mov (%rsi),%rcx
		0x48, 0x3b, 0x4e, 0x68, // cmp 104(%rsi),%rcx
		0x73, 0x0c, // jae report
		0x48, 0xff, 0x06, // incq (%rsi)
		0x48, 0x8b, 0x54, 0xce, 0x08, // mov 8(%rsi,%rcx,8),%rdx
		0x8a, 0x02, // mov (%rdx),%al
		0x0f, 0x0b, // ud2
		// report:
		0x48, 0xba, 0x00, 0x20, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, // movabs $UVA_MMIO,%rdx
		0x89, 0x0a, // mov %ecx,(%rdx)
		0x0f, 0x05, // syscall
		0x0f, 0x0b, // ud2
	}
	checkBytes(t, "umod pattern", g.mem, O_UCODE+UCODE_PATTERN, want)
	// The jae must land on the report.
	if jae := 17; int(want[jae+1])+jae+2 != 31 {
		t.Errorf("jae does not reach the report")
	}
}

func TestBuildSmodProbeCode(t *testing.T) {
	g := newTestGuest(t, false)
	entry := g.buildSmodProbeCode()
	want := []byte{
		0xb8, 0x01, 0x02, 0x00, 0x40, // mov $PVM_CPUID_FEATURES,%eax
		0x31, 0xc9, // xor %ecx,%ecx
		0x0f, 0x01, 0x3c, 0x25, 0x50, 0x56, 0x4d, 0xff, 0x0f, 0xa2, // PVM_SYNTHETIC_CPUID
		0x48, 0xbf, 0x00, 0x70, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, // movabs $kva+O_PVCS,%rdi
		0x48, 0xb8, 0x78, 0x56, 0x34, 0x12, 0x00, 0x7f, 0x00, 0x00, // movabs $CR2_PROBE,%rax
		0x48, 0x89, 0x47, 0x08, // mov %rax,cr2(%rdi)
		0x48, 0xba, 0x00, 0x00, 0x01, 0x00, 0x00, 0x40, 0x00, 0x00, // movabs $kva+0x10000,%rdx
		0x89, 0x1a, // mov %ebx,(%rdx)
		0xeb, 0xfe, // jmp .
	}
	checkBytes(t, "smod probe", g.mem, O_SCODE+0x600, want)
	if entry != 0x400000009600 {
		t.Errorf("probe entry %#x, want 0x400000009600", entry)
	}
}

func TestBuildEventCode(t *testing.T) {
	g := newTestGuest(t, false)
	event := func(marker byte) []byte {
		return []byte{
			0x48, 0xbf, 0x00, 0x70, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, // movabs $kva+O_PVCS,%rdi
			0x0f, 0xb7, 0x47, 0x46, // movzwl event_vector(%rdi),%eax
			0x0d, 0x00, 0x00, 0x00, marker, // or $marker,%eax
			0x48, 0xba, 0x00, 0x00, 0x01, 0x00, 0x00, 0x40, 0x00, 0x00, // movabs $kva+0x10000,%rdx
			0x89, 0x02, // mov %eax,(%rdx)
			0xeb, 0xfe, // jmp .
		}
	}
	checkBytes(t, "user event", g.mem, O_EVENT, event(0xe7))
	checkBytes(t, "supervisor event", g.mem, O_EVENT+512, event(0xe8))
}

func TestBuildPageTables(t *testing.T) {
	g := newTestGuest(t, false)
	want := map[[2]uint64]uint64{
		{O_PML4, 0}:       0x104007,
		{O_PML4, 128}:     0x101003,
		{O_PDP_K, 0}:      0x102003,
		{O_PD_K, 0}:       0x103003,
		{O_PT_K, 14}:      0x10e003,
		{O_PT_K, 16}:      0xd0001003,
		{O_PDP_U, 1}:      0x105007,
		{O_PD_U, 0}:       0x106007,
		{O_PT_U, 0}:       0x10b007,
		{O_PT_U, 1}:       0x10c007,
		{O_PT_U, 2}:       0xd0000007,
		{O_PML4_ALT, 128}: 0x101003,
	}
	for i := uint64(0); i < 13; i++ {
		want[[2]uint64{O_PT_K, i}] = 0x100003 + i*0x1000
	}
	for _, table := range []uint64{O_PML4, O_PDP_K, O_PD_K, O_PT_K, O_PDP_U, O_PD_U, O_PT_U, O_PML4_ALT} {
		for i := uint64(0); i < 512; i++ {
			got := binary.LittleEndian.Uint64(g.mem[table+i*8:])
			if w := want[[2]uint64{table, i}]; got != w {
				t.Errorf("table %#x[%d] = %#x, want %#x", table, i, got, w)
			}
		}
	}
	if got := binary.LittleEndian.Uint64(g.mem[O_CTRL+CTRL_PVCS:]); got != 0x400000007000 {
		t.Errorf("CTRL_PVCS %#x, want 0x400000007000", got)
	}
}

func TestCtrlSet(t *testing.T) {
	g := newTestGuest(t, false)
	g.ctrlSet(0x0000800000000000, 0x202, GOOD_SEL)
	g.ctrlSetPgds(GUEST_PHYS_BASE+O_PML4_ALT, GUEST_PHYS_BASE+O_PML4)
	want := []byte{
		0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x00, 0x00, // rip
		0x02, 0x02, 0x00, 0x00, // eflags
		0x33, 0x00, 0x2b, 0x00, // user_cs, user_ss
		0x00, 0xd0, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, // pgd1
		0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, // pgd2
		0x00, 0x70, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, // pvcs
	}
	checkBytes(t, "control page", g.mem, O_CTRL, want)
}
