// Package pvm is the PVM guest/hypervisor ABI, <asm/pvm_para.h>, restated in
// Go under the header's own names.
//
// The C tests took these from the tree under test rather than restating
// them: they used to be a hand-copied block of #defines, which is how the
// tests came to be driving MSR numbers the kernel had stopped using.  A Go
// program cannot include the header, so para_test.go does the next best
// thing: with KSRC set it parses $KSRC/arch/x86/include/uapi/asm/pvm_para.h
// and fails on any constant, and any field of struct pvm_vcpu_struct, that
// no longer matches what is written here.
package pvm

import "unsafe"

// The CPUID instruction in PVM guest might not be trapped and emulated, so
// PVM guest should use the following two instructions instead:
// "invlpg 0xffffffffff4d5650; cpuid;"
//
// PVM_SYNTHETIC_CPUID is supposed to not trigger any trap in the real or any
// paravirtual x86 kernel mode and is also guaranteed to trigger a trap in
// the underlying hardware user mode for the hypervisor emulating it.  The
// hypervisor emulates both of the basic instructions, while the INVLPG is
// often emulated as an NOP since 0xffffffffff4d5650 is normally out of the
// allowed linear address ranges.
var PVM_SYNTHETIC_CPUID = [...]byte{0x0f, 0x01, 0x3c, 0x25, 0x50, 0x56, 0x4d, 0xff, 0x0f, 0xa2}

const PVM_SYNTHETIC_CPUID_ADDRESS = 0xffffffffff4d5650

// The PVM ABI leaves.  The hypervisor CPUID class is subdivided into 0x100
// ranges, one per hypervisor sub-class; this is PVM's.
//
// PVM_CPUID_SIGNATURE:
//
//	eax = the highest PVM leaf supported
//	ebx, ecx, edx = PVM_SIGNATURE
//
// PVM_CPUID_FEATURES:
//
//	eax = PVM_ABI_VERSION, the revision of this specification implemented
//	ebx = feature bitmap, PVM_FEATURE_*
//	ecx, edx = reserved, zero
const (
	PVM_CPUID_SIGNATURE = 0x40000200
	PVM_SIGNATURE       = "PVMPVMPVM\x00\x00\x00"
	PVM_CPUID_FEATURES  = 0x40000201
	PVM_CPUID_MAX       = PVM_CPUID_FEATURES
)

// The ABI revision.  It covers everything a guest cannot discover any other
// way: the MSR numbers, the hypercall numbers, the PVCS layout, the event
// rules, the address space split.
const PVM_ABI_VERSION = 1

// PVM_FEATURE_*: bits of PVM_CPUID_FEATURES.ebx, which say what the
// hypervisor supports, and of MSR_PVM_FEATURES_ENABLED, which says what the
// guest has accepted.  A supported bit changes nothing until the guest sets
// it in the MSR.
//
// PVM_FEATURE_DIRECT_PF: the hypervisor may deliver a user-mode, not-present
// page fault without walking the guest page tables.
const (
	PVM_FEATURE_DIRECT_PF_BIT = 0
	PVM_FEATURE_DIRECT_PF     = 1 << PVM_FEATURE_DIRECT_PF_BIT
)

// PVM virtual MSRs, 0x4b564d20-0x4b564d2f.
const (
	PVM_VIRTUAL_MSR_BASE   = 0x4b564d20
	PVM_VIRTUAL_MSR_MAX_NR = 16
	PVM_VIRTUAL_MSR_MAX    = PVM_VIRTUAL_MSR_BASE + PVM_VIRTUAL_MSR_MAX_NR - 1

	MSR_PVM_VCPU_STRUCT      = PVM_VIRTUAL_MSR_BASE + 0
	MSR_PVM_EVENT_ENTRY      = PVM_VIRTUAL_MSR_BASE + 1
	MSR_PVM_RETU_RIP         = PVM_VIRTUAL_MSR_BASE + 2
	MSR_PVM_FEATURES_ENABLED = PVM_VIRTUAL_MSR_BASE + 3
)

// Hypercalls.
const (
	PVM_HC_SPECIAL_MAX_NR = 256
	PVM_HC_SPECIAL_BASE   = 0x17088200
	PVM_HC_SPECIAL_MAX    = PVM_HC_SPECIAL_BASE + PVM_HC_SPECIAL_MAX_NR

	PVM_HC_LOAD_PGTBL        = PVM_HC_SPECIAL_BASE + 0
	PVM_HC_IRQ_WIN           = PVM_HC_SPECIAL_BASE + 1
	PVM_HC_IRQ_HALT          = PVM_HC_SPECIAL_BASE + 2
	PVM_HC_TLB_FLUSH         = PVM_HC_SPECIAL_BASE + 3
	PVM_HC_TLB_FLUSH_CURRENT = PVM_HC_SPECIAL_BASE + 4
	PVM_HC_TLB_INVLPG        = PVM_HC_SPECIAL_BASE + 5
	PVM_HC_LOAD_GS           = PVM_HC_SPECIAL_BASE + 6
	PVM_HC_RDMSR             = PVM_HC_SPECIAL_BASE + 7
	PVM_HC_WRMSR             = PVM_HC_SPECIAL_BASE + 8
	PVM_HC_LOAD_TLS          = PVM_HC_SPECIAL_BASE + 9
)

// PVM_EVENT_FLAGS_IF: interrupt enable flag.  PVM_EVENT_FLAGS_IP: interrupt
// pending flag, set by the hypervisor if it fails to inject a maskable event
// because the interrupt-enable flag is clear in supervisor mode.
const (
	PVM_EVENT_FLAGS_IP_BIT = 8
	PVM_EVENT_FLAGS_IP     = 1 << PVM_EVENT_FLAGS_IP_BIT
	PVM_EVENT_FLAGS_IF_BIT = 9
	PVM_EVENT_FLAGS_IF     = 1 << PVM_EVENT_FLAGS_IF_BIT
)

// Bits for event_vector.  The lowest 8 bits are the vector number for non
// async-exception events; PVM_PVCS_EVENT_VECTOR_STD is set when a non
// async-exception is delivered, NMI and MCE mark those being delivered or
// pending.
const (
	PVM_PVCS_EVENT_VECTOR_STD_BIT = 8
	PVM_PVCS_EVENT_VECTOR_STD     = 1 << PVM_PVCS_EVENT_VECTOR_STD_BIT
	PVM_PVCS_EVENT_VECTOR_NMI_BIT = 9
	PVM_PVCS_EVENT_VECTOR_NMI     = 1 << PVM_PVCS_EVENT_VECTOR_NMI_BIT
	PVM_PVCS_EVENT_VECTOR_MCE_BIT = 10
	PVM_PVCS_EVENT_VECTOR_MCE     = 1 << PVM_PVCS_EVENT_VECTOR_MCE_BIT
)

const (
	PVM_LOAD_PGTBL_FLAGS_TLB  = 1 << 0
	PVM_LOAD_PGTBL_FLAGS_LA57 = 1 << 1
)

// VcpuStruct is struct pvm_vcpu_struct, the PVCS.  PVM event delivery saves
// the information about the event and the old context into it if the event
// is from user mode or from supervisor mode with vector >= 32, and the ERETU
// synthetic instruction reads the return state from it.
//
// The c tags are the header's field names; para_test.go matches on them.
type VcpuStruct struct {
	// Only used in supervisor mode, with only bits 8 and 9 valid.
	EventFlags uint64    `c:"event_flags"`
	CR2        uint64    `c:"cr2"`
	Reserved0  [6]uint64 `c:"reserved0"`

	// For the event from supervisor mode, user_cs, user_ss, user_gsbase
	// and pkru are ignored and kept untouched.
	UserCS       uint16    `c:"user_cs"`
	UserSS       uint16    `c:"user_ss"`
	EventErrcode uint16    `c:"event_errcode"`
	EventVector  uint16    `c:"event_vector"`
	UserGSBase   uint64    `c:"user_gsbase"`
	EFlags       uint32    `c:"eflags"`
	PKRU         uint32    `c:"pkru"`
	RIP          uint64    `c:"rip"`
	RCX          uint64    `c:"rcx"`
	R11          uint64    `c:"r11"`
	Reserved1    [2]uint64 `c:"reserved1"`
}

// Offsets into the PVCS, taken from the struct rather than written out: the
// switcher test's guest builds its PVCS as raw bytes, so a field moving would
// otherwise leave these pointing somewhere plausible and wrong.
const (
	PVCS_EVENT_FLAGS   = unsafe.Offsetof(VcpuStruct{}.EventFlags)
	PVCS_CR2           = unsafe.Offsetof(VcpuStruct{}.CR2)
	PVCS_USER_CS       = unsafe.Offsetof(VcpuStruct{}.UserCS)
	PVCS_USER_SS       = unsafe.Offsetof(VcpuStruct{}.UserSS)
	PVCS_EVENT_ERRCODE = unsafe.Offsetof(VcpuStruct{}.EventErrcode)
	PVCS_EVENT_VECTOR  = unsafe.Offsetof(VcpuStruct{}.EventVector)
	PVCS_USER_GSBASE   = unsafe.Offsetof(VcpuStruct{}.UserGSBase)
	PVCS_EFLAGS        = unsafe.Offsetof(VcpuStruct{}.EFlags)
	PVCS_PKRU          = unsafe.Offsetof(VcpuStruct{}.PKRU)
	PVCS_RIP           = unsafe.Offsetof(VcpuStruct{}.RIP)
	PVCS_RCX           = unsafe.Offsetof(VcpuStruct{}.RCX)
	PVCS_R11           = unsafe.Offsetof(VcpuStruct{}.R11)
	PVCS_SIZE          = unsafe.Sizeof(VcpuStruct{})
)
