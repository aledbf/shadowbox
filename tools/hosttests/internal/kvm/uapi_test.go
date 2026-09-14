package kvm

import (
	"testing"
	"unsafe"
)

// The expected values below are what gcc reports for <linux/kvm.h> on
// x86_64 (sizeof/offsetof of each structure and the ioctl macros).

func TestSizes(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"kvm_regs", unsafe.Sizeof(Regs{}), 144},
		{"kvm_segment", unsafe.Sizeof(Segment{}), 24},
		{"kvm_dtable", unsafe.Sizeof(DTable{}), 16},
		{"kvm_sregs", unsafe.Sizeof(Sregs{}), 312},
		{"kvm_msrs", unsafe.Sizeof(MSRs{}), 8},
		{"kvm_msr_entry", unsafe.Sizeof(MSREntry{}), 16},
		{"kvm_userspace_memory_region", unsafe.Sizeof(UserspaceMemoryRegion{}), 32},
		{"kvm_cpuid2", unsafe.Sizeof(CPUID2{}), 8},
		{"kvm_cpuid_entry2", unsafe.Sizeof(CPUIDEntry2{}), 40},
		{"kvm_xsave", unsafe.Sizeof(XSave{}), 4096},
		{"kvm_stats_header", unsafe.Sizeof(StatsHeader{}), 24},
		{"kvm_stats_desc", unsafe.Sizeof(StatsDesc{}), 16},
		// Up to the end of the exit union, which is where kvm_valid_regs is.
		{"kvm_run (to kvm_valid_regs)", unsafe.Sizeof(Run{}), 288},
		{"kvm_run.mmio", unsafe.Sizeof(RunMMIO{}), 24},
		{"kvm_run.internal (to data)", unsafe.Sizeof(RunInternal{}), 8},

		{"sizeofCPUID2", sizeofCPUID2, unsafe.Sizeof(CPUID2{})},
		{"sizeofMemRegion", sizeofMemRegion, unsafe.Sizeof(UserspaceMemoryRegion{})},
		{"sizeofRegs", sizeofRegs, unsafe.Sizeof(Regs{})},
		{"sizeofSregs", sizeofSregs, unsafe.Sizeof(Sregs{})},
		{"sizeofMSRs", sizeofMSRs, unsafe.Sizeof(MSRs{})},
		{"sizeofXSave", sizeofXSave, unsafe.Sizeof(XSave{})},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("sizeof %s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestOffsets(t *testing.T) {
	var (
		r   Regs
		seg Segment
		dt  DTable
		s   Sregs
		m   MSRs
		me  MSREntry
		mr  UserspaceMemoryRegion
		c   CPUID2
		ce  CPUIDEntry2
		x   XSave
		sh  StatsHeader
		sd  StatsDesc
		run Run
		mm  RunMMIO
		in  RunInternal
	)
	const union = 32 // offsetof(struct kvm_run, mmio)
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"kvm_regs.rax", unsafe.Offsetof(r.RAX), 0},
		{"kvm_regs.rbx", unsafe.Offsetof(r.RBX), 8},
		{"kvm_regs.rcx", unsafe.Offsetof(r.RCX), 16},
		{"kvm_regs.rdx", unsafe.Offsetof(r.RDX), 24},
		{"kvm_regs.rsi", unsafe.Offsetof(r.RSI), 32},
		{"kvm_regs.rdi", unsafe.Offsetof(r.RDI), 40},
		{"kvm_regs.rsp", unsafe.Offsetof(r.RSP), 48},
		{"kvm_regs.rbp", unsafe.Offsetof(r.RBP), 56},
		{"kvm_regs.r8", unsafe.Offsetof(r.R8), 64},
		{"kvm_regs.r15", unsafe.Offsetof(r.R15), 120},
		{"kvm_regs.rip", unsafe.Offsetof(r.RIP), 128},
		{"kvm_regs.rflags", unsafe.Offsetof(r.RFLAGS), 136},

		{"kvm_segment.base", unsafe.Offsetof(seg.Base), 0},
		{"kvm_segment.limit", unsafe.Offsetof(seg.Limit), 8},
		{"kvm_segment.selector", unsafe.Offsetof(seg.Selector), 12},
		{"kvm_segment.type", unsafe.Offsetof(seg.Type), 14},
		{"kvm_segment.present", unsafe.Offsetof(seg.Present), 15},
		{"kvm_segment.dpl", unsafe.Offsetof(seg.DPL), 16},
		{"kvm_segment.db", unsafe.Offsetof(seg.DB), 17},
		{"kvm_segment.s", unsafe.Offsetof(seg.S), 18},
		{"kvm_segment.l", unsafe.Offsetof(seg.L), 19},
		{"kvm_segment.g", unsafe.Offsetof(seg.G), 20},
		{"kvm_segment.avl", unsafe.Offsetof(seg.AVL), 21},
		{"kvm_segment.unusable", unsafe.Offsetof(seg.Unusable), 22},
		{"kvm_segment.padding", unsafe.Offsetof(seg.Padding), 23},

		{"kvm_dtable.base", unsafe.Offsetof(dt.Base), 0},
		{"kvm_dtable.limit", unsafe.Offsetof(dt.Limit), 8},
		{"kvm_dtable.padding", unsafe.Offsetof(dt.Padding), 10},

		{"kvm_sregs.cs", unsafe.Offsetof(s.CS), 0},
		{"kvm_sregs.ds", unsafe.Offsetof(s.DS), 24},
		{"kvm_sregs.es", unsafe.Offsetof(s.ES), 48},
		{"kvm_sregs.fs", unsafe.Offsetof(s.FS), 72},
		{"kvm_sregs.gs", unsafe.Offsetof(s.GS), 96},
		{"kvm_sregs.ss", unsafe.Offsetof(s.SS), 120},
		{"kvm_sregs.tr", unsafe.Offsetof(s.TR), 144},
		{"kvm_sregs.ldt", unsafe.Offsetof(s.LDT), 168},
		{"kvm_sregs.gdt", unsafe.Offsetof(s.GDT), 192},
		{"kvm_sregs.idt", unsafe.Offsetof(s.IDT), 208},
		{"kvm_sregs.cr0", unsafe.Offsetof(s.CR0), 224},
		{"kvm_sregs.cr2", unsafe.Offsetof(s.CR2), 232},
		{"kvm_sregs.cr3", unsafe.Offsetof(s.CR3), 240},
		{"kvm_sregs.cr4", unsafe.Offsetof(s.CR4), 248},
		{"kvm_sregs.cr8", unsafe.Offsetof(s.CR8), 256},
		{"kvm_sregs.efer", unsafe.Offsetof(s.EFER), 264},
		{"kvm_sregs.apic_base", unsafe.Offsetof(s.APICBase), 272},
		{"kvm_sregs.interrupt_bitmap", unsafe.Offsetof(s.InterruptBitmap), 280},

		{"kvm_msrs.nmsrs", unsafe.Offsetof(m.NMSRs), 0},
		{"kvm_msrs.pad", unsafe.Offsetof(m.Pad), 4},
		{"kvm_msr_entry.index", unsafe.Offsetof(me.Index), 0},
		{"kvm_msr_entry.reserved", unsafe.Offsetof(me.Reserved), 4},
		{"kvm_msr_entry.data", unsafe.Offsetof(me.Data), 8},

		{"kvm_userspace_memory_region.slot", unsafe.Offsetof(mr.Slot), 0},
		{"kvm_userspace_memory_region.flags", unsafe.Offsetof(mr.Flags), 4},
		{"kvm_userspace_memory_region.guest_phys_addr", unsafe.Offsetof(mr.GuestPhysAddr), 8},
		{"kvm_userspace_memory_region.memory_size", unsafe.Offsetof(mr.MemorySize), 16},
		{"kvm_userspace_memory_region.userspace_addr", unsafe.Offsetof(mr.UserspaceAddr), 24},

		{"kvm_cpuid2.nent", unsafe.Offsetof(c.Nent), 0},
		{"kvm_cpuid2.padding", unsafe.Offsetof(c.Padding), 4},
		{"kvm_cpuid_entry2.function", unsafe.Offsetof(ce.Function), 0},
		{"kvm_cpuid_entry2.index", unsafe.Offsetof(ce.Index), 4},
		{"kvm_cpuid_entry2.flags", unsafe.Offsetof(ce.Flags), 8},
		{"kvm_cpuid_entry2.eax", unsafe.Offsetof(ce.EAX), 12},
		{"kvm_cpuid_entry2.ebx", unsafe.Offsetof(ce.EBX), 16},
		{"kvm_cpuid_entry2.ecx", unsafe.Offsetof(ce.ECX), 20},
		{"kvm_cpuid_entry2.edx", unsafe.Offsetof(ce.EDX), 24},
		{"kvm_cpuid_entry2.padding", unsafe.Offsetof(ce.Padding), 28},

		{"kvm_xsave.region", unsafe.Offsetof(x.Region), 0},

		{"kvm_stats_header.flags", unsafe.Offsetof(sh.Flags), 0},
		{"kvm_stats_header.name_size", unsafe.Offsetof(sh.NameSize), 4},
		{"kvm_stats_header.num_desc", unsafe.Offsetof(sh.NumDesc), 8},
		{"kvm_stats_header.id_offset", unsafe.Offsetof(sh.IDOffset), 12},
		{"kvm_stats_header.desc_offset", unsafe.Offsetof(sh.DescOffset), 16},
		{"kvm_stats_header.data_offset", unsafe.Offsetof(sh.DataOffset), 20},
		{"kvm_stats_desc.flags", unsafe.Offsetof(sd.Flags), 0},
		{"kvm_stats_desc.exponent", unsafe.Offsetof(sd.Exponent), 4},
		{"kvm_stats_desc.size", unsafe.Offsetof(sd.Size), 6},
		{"kvm_stats_desc.offset", unsafe.Offsetof(sd.Offset), 8},
		{"kvm_stats_desc.bucket_size", unsafe.Offsetof(sd.BucketSize), 12},

		{"kvm_run.request_interrupt_window", unsafe.Offsetof(run.RequestInterruptWindow), 0},
		{"kvm_run.immediate_exit", unsafe.Offsetof(run.ImmediateExit), 1},
		{"kvm_run.exit_reason", unsafe.Offsetof(run.ExitReason), 8},
		{"kvm_run.ready_for_interrupt_injection", unsafe.Offsetof(run.ReadyForInterruptInjection), 12},
		{"kvm_run.if_flag", unsafe.Offsetof(run.IFFlag), 13},
		{"kvm_run.flags", unsafe.Offsetof(run.Flags), 14},
		{"kvm_run.cr8", unsafe.Offsetof(run.CR8), 16},
		{"kvm_run.apic_base", unsafe.Offsetof(run.APICBase), 24},
		{"kvm_run.mmio", unsafe.Offsetof(run.exit), 32},
		{"kvm_run.mmio.phys_addr", union + unsafe.Offsetof(mm.PhysAddr), 32},
		{"kvm_run.mmio.data", union + unsafe.Offsetof(mm.Data), 40},
		{"kvm_run.mmio.len", union + unsafe.Offsetof(mm.Len), 48},
		{"kvm_run.mmio.is_write", union + unsafe.Offsetof(mm.IsWrite), 52},
		{"kvm_run.internal.suberror", union + unsafe.Offsetof(in.Suberror), 32},
		{"kvm_run.internal.ndata", union + unsafe.Offsetof(in.NData), 36},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("offsetof %s = %d, want %d", c.name, c.got, c.want)
		}
	}

	// The accessors must land on the union.
	base := uintptr(unsafe.Pointer(&run))
	if got := uintptr(unsafe.Pointer(run.MMIO())) - base; got != 32 {
		t.Errorf("Run.MMIO() at offset %d, want 32", got)
	}
	if got := uintptr(unsafe.Pointer(run.Internal())) - base; got != 32 {
		t.Errorf("Run.Internal() at offset %d, want 32", got)
	}
}

func TestIoctlNumbers(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"KVM_CREATE_VM", CREATE_VM, 0xae01},
		{"KVM_CHECK_EXTENSION", CHECK_EXTENSION, 0xae03},
		{"KVM_GET_VCPU_MMAP_SIZE", GET_VCPU_MMAP_SIZE, 0xae04},
		{"KVM_GET_SUPPORTED_CPUID", GET_SUPPORTED_CPUID, 0xc008ae05},
		{"KVM_CREATE_VCPU", CREATE_VCPU, 0xae41},
		{"KVM_SET_USER_MEMORY_REGION", SET_USER_MEMORY_REGION, 0x4020ae46},
		{"KVM_RUN", RUN, 0xae80},
		{"KVM_GET_REGS", GET_REGS, 0x8090ae81},
		{"KVM_SET_REGS", SET_REGS, 0x4090ae82},
		{"KVM_GET_SREGS", GET_SREGS, 0x8138ae83},
		{"KVM_SET_SREGS", SET_SREGS, 0x4138ae84},
		{"KVM_GET_MSRS", GET_MSRS, 0xc008ae88},
		{"KVM_SET_MSRS", SET_MSRS, 0x4008ae89},
		{"KVM_SET_CPUID2", SET_CPUID2, 0x4008ae90},
		{"KVM_GET_XSAVE", GET_XSAVE, 0x9000aea4},
		{"KVM_GET_STATS_FD", GET_STATS_FD, 0xaece},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}

func TestConstants(t *testing.T) {
	cases := []struct {
		name      string
		got, want int
	}{
		{"KVM_EXIT_UNKNOWN", EXIT_UNKNOWN, 0},
		{"KVM_EXIT_EXCEPTION", EXIT_EXCEPTION, 1},
		{"KVM_EXIT_IO", EXIT_IO, 2},
		{"KVM_EXIT_HLT", EXIT_HLT, 5},
		{"KVM_EXIT_MMIO", EXIT_MMIO, 6},
		{"KVM_EXIT_SHUTDOWN", EXIT_SHUTDOWN, 8},
		{"KVM_EXIT_FAIL_ENTRY", EXIT_FAIL_ENTRY, 9},
		{"KVM_EXIT_INTR", EXIT_INTR, 10},
		{"KVM_EXIT_INTERNAL_ERROR", EXIT_INTERNAL_ERROR, 17},
		{"KVM_CAP_MAX_VCPUS", CAP_MAX_VCPUS, 66},
		{"KVM_CPUID_FLAG_SIGNIFCANT_INDEX", CPUID_FLAG_SIGNIFCANT_INDEX, 1},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}
