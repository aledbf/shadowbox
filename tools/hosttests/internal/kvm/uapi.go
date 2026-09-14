// Package kvm is the part of <linux/kvm.h> the host tests use, restated with
// the x86_64 layouts.  uapi_test.go holds every size and offset to the values
// the C compiler gives for the real header, so a mistake here is a test
// failure rather than an ioctl scribbling past a structure.
package kvm

import "unsafe"

// The _IOC encoding from <asm-generic/ioctl.h>, which x86 uses unchanged.
const (
	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits

	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<iocDirShift | typ<<iocTypeShift | nr<<iocNRShift | size<<iocSizeShift
}

// IO, IOR, IOW and IOWR are the _IO* macros.
func IO(typ, nr uintptr) uintptr         { return ioc(iocNone, typ, nr, 0) }
func IOR(typ, nr, size uintptr) uintptr  { return ioc(iocRead, typ, nr, size) }
func IOW(typ, nr, size uintptr) uintptr  { return ioc(iocWrite, typ, nr, size) }
func IOWR(typ, nr, size uintptr) uintptr { return ioc(iocRead|iocWrite, typ, nr, size) }

// KVMIO is the ioctl type byte of every KVM ioctl.
const KVMIO = 0xAE

// struct kvm_regs
type Regs struct {
	RAX, RBX, RCX, RDX uint64
	RSI, RDI, RSP, RBP uint64
	R8, R9, R10, R11   uint64
	R12, R13, R14, R15 uint64
	RIP, RFLAGS        uint64
}

// struct kvm_segment
type Segment struct {
	Base     uint64
	Limit    uint32
	Selector uint16
	Type     uint8
	Present  uint8
	DPL      uint8
	DB       uint8
	S        uint8
	L        uint8
	G        uint8
	AVL      uint8
	Unusable uint8
	Padding  uint8
}

// struct kvm_dtable
type DTable struct {
	Base    uint64
	Limit   uint16
	Padding [3]uint16
}

// KVM_NR_INTERRUPTS
const NR_INTERRUPTS = 256

// struct kvm_sregs
type Sregs struct {
	CS, DS, ES, FS, GS, SS Segment
	TR, LDT                Segment
	GDT, IDT               DTable
	CR0, CR2, CR3, CR4     uint64
	CR8                    uint64
	EFER                   uint64
	APICBase               uint64
	InterruptBitmap        [(NR_INTERRUPTS + 63) / 64]uint64
}

// struct kvm_msrs, without the flexible entries[] array.
type MSRs struct {
	NMSRs uint32
	Pad   uint32
}

// struct kvm_msr_entry
type MSREntry struct {
	Index    uint32
	Reserved uint32
	Data     uint64
}

// struct kvm_userspace_memory_region
type UserspaceMemoryRegion struct {
	Slot          uint32
	Flags         uint32
	GuestPhysAddr uint64
	MemorySize    uint64
	UserspaceAddr uint64
}

// struct kvm_cpuid2, without the flexible entries[] array.
type CPUID2 struct {
	Nent    uint32
	Padding uint32
}

// struct kvm_cpuid_entry2
type CPUIDEntry2 struct {
	Function uint32
	Index    uint32
	Flags    uint32
	EAX      uint32
	EBX      uint32
	ECX      uint32
	EDX      uint32
	Padding  [3]uint32
}

// KVM_CPUID_FLAG_SIGNIFCANT_INDEX (sic)
const CPUID_FLAG_SIGNIFCANT_INDEX = 1 << 0

// struct kvm_xsave, without the flexible extra[] array.
type XSave struct {
	Region [1024]uint32
}

// struct kvm_stats_header
type StatsHeader struct {
	Flags      uint32
	NameSize   uint32
	NumDesc    uint32
	IDOffset   uint32
	DescOffset uint32
	DataOffset uint32
}

// struct kvm_stats_desc, without the flexible name[] array.
type StatsDesc struct {
	Flags      uint32
	Exponent   int16
	Size       uint16
	Offset     uint32
	BucketSize uint32
}

// struct kvm_run, up to and including the exit union.  Everything after it
// (kvm_valid_regs, kvm_dirty_regs, the shared regs) is never touched here.
type Run struct {
	RequestInterruptWindow     uint8
	ImmediateExit              uint8
	_                          [6]uint8
	ExitReason                 uint32
	ReadyForInterruptInjection uint8
	IFFlag                     uint8
	Flags                      uint16
	CR8                        uint64
	APICBase                   uint64
	exit                       [256]byte
}

// The mmio member of the kvm_run exit union.
type RunMMIO struct {
	PhysAddr uint64
	Data     [8]uint8
	Len      uint32
	IsWrite  uint8
}

// The internal member of the kvm_run exit union, up to ndata.
type RunInternal struct {
	Suberror uint32
	NData    uint32
}

// MMIO is run->mmio.
func (r *Run) MMIO() *RunMMIO { return (*RunMMIO)(unsafe.Pointer(&r.exit)) }

// Internal is run->internal.
func (r *Run) Internal() *RunInternal { return (*RunInternal)(unsafe.Pointer(&r.exit)) }

// Exit reasons.
const (
	EXIT_UNKNOWN        = 0
	EXIT_EXCEPTION      = 1
	EXIT_IO             = 2
	EXIT_HLT            = 5
	EXIT_MMIO           = 6
	EXIT_SHUTDOWN       = 8
	EXIT_FAIL_ENTRY     = 9
	EXIT_INTR           = 10
	EXIT_INTERNAL_ERROR = 17
)

// KVM_CAP_MAX_VCPUS
const CAP_MAX_VCPUS = 66

// Sizes used in the ioctl numbers, spelled out so the numbers are constants
// that do not depend on the Go types above; uapi_test.go checks both agree.
const (
	sizeofCPUID2    = 8
	sizeofMemRegion = 32
	sizeofRegs      = 144
	sizeofSregs     = 312
	sizeofMSRs      = 8
	sizeofXSave     = 4096
)

// The ioctls.
var (
	CREATE_VM              = IO(KVMIO, 0x01)
	CHECK_EXTENSION        = IO(KVMIO, 0x03)
	GET_VCPU_MMAP_SIZE     = IO(KVMIO, 0x04)
	GET_SUPPORTED_CPUID    = IOWR(KVMIO, 0x05, sizeofCPUID2)
	CREATE_VCPU            = IO(KVMIO, 0x41)
	SET_USER_MEMORY_REGION = IOW(KVMIO, 0x46, sizeofMemRegion)
	RUN                    = IO(KVMIO, 0x80)
	GET_REGS               = IOR(KVMIO, 0x81, sizeofRegs)
	SET_REGS               = IOW(KVMIO, 0x82, sizeofRegs)
	GET_SREGS              = IOR(KVMIO, 0x83, sizeofSregs)
	SET_SREGS              = IOW(KVMIO, 0x84, sizeofSregs)
	GET_MSRS               = IOWR(KVMIO, 0x88, sizeofMSRs)
	SET_MSRS               = IOW(KVMIO, 0x89, sizeofMSRs)
	SET_CPUID2             = IOW(KVMIO, 0x90, sizeofCPUID2)
	GET_XSAVE              = IOR(KVMIO, 0xa4, sizeofXSave)
	GET_STATS_FD           = IO(KVMIO, 0xce)
)
