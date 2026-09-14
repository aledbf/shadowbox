package harness

import (
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/aledbf/pvm-testbed/tools/hosttests/internal/kvm"
)

// X86_CR4_PKE
const X86_CR4_PKE = 1 << 22

const (
	GUEST_PHYS_BASE = 0x100000
	GUEST_MEM_SIZE  = 8 << 20
	PVCS_GPA        = GUEST_PHYS_BASE + 0x2000
)

// MAX_CPUID_ENTRIES is how many entries KVM_GET_SUPPORTED_CPUID may return.
const MAX_CPUID_ENTRIES = 256

// CPUID2 is a struct kvm_cpuid2 with room for MAX_CPUID_ENTRIES entries.
type CPUID2 struct {
	kvm.CPUID2
	Entries [MAX_CPUID_ENTRIES]kvm.CPUIDEntry2
}

// VM is one VM with one vCPU and GUEST_MEM_SIZE of memory at
// GUEST_PHYS_BASE.
type VM struct {
	KVM, VM, VCPU int
	Run           []byte // the vCPU's kvm_run mapping
	RunSize       int
	Mem           []byte
	CPUID         *CPUID2 // what KVM_GET_SUPPORTED_CPUID said
}

// RunData is the kvm_run structure behind v.Run.
func (v *VM) RunData() *kvm.Run {
	return (*kvm.Run)(unsafe.Pointer(&v.Run[0]))
}

// MemAddr is the host virtual address of guest memory, for a memslot.
func (v *VM) MemAddr() uintptr {
	return uintptr(unsafe.Pointer(&v.Mem[0]))
}

// Ioctl is ioctl(2) with a pointer argument.  It returns the ioctl's
// return value, or -1 and the errno.
func Ioctl(fd int, req uintptr, arg unsafe.Pointer) (int, syscall.Errno) {
	r, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if e != 0 {
		return -1, e
	}
	return int(r), 0
}

// IoctlInt is ioctl(2) with an integer argument.
func IoctlInt(fd int, req uintptr, arg uintptr) (int, syscall.Errno) {
	r, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg)
	if e != 0 {
		return -1, e
	}
	return int(r), 0
}

// KVMRun is ioctl(vcpu, KVM_RUN, 0).  The C tests had no signal handlers
// running, so KVM_RUN never returned EINTR there; the Go runtime does send
// its threads signals, and an EINTR that no one asked for is retried rather
// than reported.
func KVMRun(vcpu int) (int, syscall.Errno) {
	for {
		r, e := IoctlInt(vcpu, kvm.RUN, 0)
		if e != syscall.EINTR {
			return r, e
		}
	}
}

// ExitReasonName names a KVM exit reason the way the C harness did.
func ExitReasonName(r int) string {
	switch r {
	case kvm.EXIT_UNKNOWN:
		return "UNKNOWN"
	case kvm.EXIT_EXCEPTION:
		return "EXCEPTION"
	case kvm.EXIT_IO:
		return "IO"
	case kvm.EXIT_HLT:
		return "HLT"
	case kvm.EXIT_MMIO:
		return "MMIO"
	case kvm.EXIT_SHUTDOWN:
		return "SHUTDOWN"
	case kvm.EXIT_FAIL_ENTRY:
		return "FAIL_ENTRY"
	case kvm.EXIT_INTR:
		return "INTR"
	case kvm.EXIT_INTERNAL_ERROR:
		return "INTERNAL_ERROR"
	default:
		return "?"
	}
}

// getSupportedCPUID is the CPUID KVM says it supports, which for a PVM host
// is what PVM chose to advertise in pvm_set_cpu_caps().  Also what a VMM
// hands the vCPU, and without it the vCPU has no guest_cpu_cap at all --
// CR4.PKE then reads as a reserved bit and KVM_SET_SREGS refuses it.
func (v *VM) getSupportedCPUID() *CPUID2 {
	c := new(CPUID2)
	c.Nent = MAX_CPUID_ENTRIES
	if _, e := Ioctl(v.KVM, kvm.GET_SUPPORTED_CPUID, unsafe.Pointer(c)); e != 0 {
		Die("KVM_GET_SUPPORTED_CPUID", e)
	}
	return c
}

// CPUIDHas says whether bit @bit of register @reg (0 eax, 1 ebx, 2 ecx,
// 3 edx) is set in leaf @function, subleaf @index.
func CPUIDHas(c *CPUID2, function, index uint32, reg int, bit uint) bool {
	for i := uint32(0); i < c.Nent; i++ {
		e := &c.Entries[i]
		if e.Function != function {
			continue
		}
		if e.Flags&kvm.CPUID_FLAG_SIGNIFCANT_INDEX != 0 && e.Index != index {
			continue
		}
		regs := [4]uint32{e.EAX, e.EBX, e.ECX, e.EDX}
		return regs[reg]&(1<<bit) != 0
	}
	return false
}

type msrList struct {
	hdr   kvm.MSRs
	entry kvm.MSREntry
}

// SetMSR writes one MSR.  KVM_SET_MSRS returns how many entries it managed
// to write, so a return of 0 for a single-entry list is the vendor's
// set_msr() having said no.  A negative return is the ioctl itself failing
// -- minus the errno -- which is a different thing and worth distinguishing
// in the message.
func (v *VM) SetMSR(index uint32, data uint64) int {
	m := msrList{hdr: kvm.MSRs{NMSRs: 1}, entry: kvm.MSREntry{Index: index, Data: data}}
	r, e := Ioctl(v.VCPU, kvm.SET_MSRS, unsafe.Pointer(&m))
	if r < 0 {
		return -int(e)
	}
	return r
}

// GetMSR reads one MSR into *data, returning 1 if it did, 0 if the vendor
// refused, and minus the errno if the ioctl failed.  *data is left alone
// unless the return is 1.
func (v *VM) GetMSR(index uint32, data *uint64) int {
	m := msrList{hdr: kvm.MSRs{NMSRs: 1}, entry: kvm.MSREntry{Index: index}}
	r, e := Ioctl(v.VCPU, kvm.GET_MSRS, unsafe.Pointer(&m))
	if r < 0 {
		return -int(e)
	}
	if r != 1 {
		return 0
	}
	*data = m.entry.Data
	return 1
}

// SetMemslot is KVM_SET_USER_MEMORY_REGION; 0 or minus the errno.
func (v *VM) SetMemslot(slot uint32, gpa, size uint64, hva uintptr, flags uint32) int {
	r := kvm.UserspaceMemoryRegion{
		Slot:          slot,
		Flags:         flags,
		GuestPhysAddr: gpa,
		MemorySize:    size,
		UserspaceAddr: uint64(hva),
	}
	if _, e := Ioctl(v.VM, kvm.SET_USER_MEMORY_REGION, unsafe.Pointer(&r)); e != 0 {
		return -int(e)
	}
	return 0
}

// MmapRun maps @fd's kvm_run.
func (v *VM) MmapRun(fd int) error {
	b, err := syscall.Mmap(fd, 0, v.RunSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		v.Run = nil
		return err
	}
	v.Run = b
	return nil
}

// UnmapRun unmaps the vCPU's kvm_run, if it is mapped.
func (v *VM) UnmapRun() {
	if v.Run != nil {
		syscall.Munmap(v.Run)
		v.Run = nil
	}
}

// Setup creates the VM, its memory and its vCPU, or dies.
func (v *VM) Setup() {
	var e syscall.Errno
	var err error

	v.KVM, err = syscall.Open("/dev/kvm", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		Die("open /dev/kvm", err)
	}

	v.VM, e = IoctlInt(v.KVM, kvm.CREATE_VM, 0)
	if v.VM < 0 {
		Die("KVM_CREATE_VM", e)
	}

	v.Mem, err = syscall.Mmap(-1, 0, GUEST_MEM_SIZE, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS|syscall.MAP_NORESERVE)
	if err != nil {
		Die("mmap guest memory", err)
	}

	if r := v.SetMemslot(0, GUEST_PHYS_BASE, GUEST_MEM_SIZE, v.MemAddr(), 0); r != 0 {
		Die("KVM_SET_USER_MEMORY_REGION", syscall.Errno(-r))
	}

	v.VCPU, e = IoctlInt(v.VM, kvm.CREATE_VCPU, 0)
	if v.VCPU < 0 {
		Die("KVM_CREATE_VCPU", e)
	}

	v.CPUID = v.getSupportedCPUID()
	if _, e := Ioctl(v.VCPU, kvm.SET_CPUID2, unsafe.Pointer(v.CPUID)); e != 0 {
		Die("KVM_SET_CPUID2", e)
	}

	v.RunSize, e = IoctlInt(v.KVM, kvm.GET_VCPU_MMAP_SIZE, 0)
	if err := v.MmapRun(v.VCPU); err != nil {
		Die("mmap kvm_run", err)
	}
}

// Teardown undoes Setup.
func (v *VM) Teardown() {
	v.UnmapRun()
	syscall.Close(v.VCPU)
	syscall.Close(v.VM)
	syscall.Munmap(v.Mem)
	v.Mem = nil
	v.CPUID = nil
	syscall.Close(v.KVM)
}

// Stat reads the vCPU binary stat called @want into *out.  It returns nil
// if it did, and otherwise the errno that stopped it (0 if no call failed
// but the stat was not there).
func (v *VM) Stat(want string, out *uint64) error {
	fd, e := IoctlInt(v.VCPU, kvm.GET_STATS_FD, 0)
	if fd < 0 {
		return e
	}
	defer syscall.Close(fd)

	var h kvm.StatsHeader
	hb := unsafe.Slice((*byte)(unsafe.Pointer(&h)), unsafe.Sizeof(h))
	if n, err := syscall.Pread(fd, hb, 0); n != len(hb) {
		return errnoOf(err)
	}

	dsz := int(unsafe.Sizeof(kvm.StatsDesc{})) + int(h.NameSize)
	d := make([]byte, dsz)
	nameOff := int(unsafe.Sizeof(kvm.StatsDesc{}))
	for i := uint32(0); i < h.NumDesc; i++ {
		n, err := syscall.Pread(fd, d, int64(h.DescOffset)+int64(i)*int64(dsz))
		if n != dsz {
			return errnoOf(err)
		}
		name := d[nameOff:]
		if j := strings.IndexByte(string(name), 0); j >= 0 {
			name = name[:j]
		}
		if string(name) != want {
			continue
		}
		desc := (*kvm.StatsDesc)(unsafe.Pointer(&d[0]))
		ob := unsafe.Slice((*byte)(unsafe.Pointer(out)), 8)
		if n, err := syscall.Pread(fd, ob, int64(h.DataOffset)+int64(desc.Offset)); n != 8 {
			return errnoOf(err)
		}
		return nil
	}
	return syscall.Errno(0)
}

func errnoOf(err error) error {
	if err == nil {
		return syscall.Errno(0)
	}
	return err
}

// IsPVMHost says whether kvm_pvm is loaded.  Every case in these programs
// is a negative test whose real assertion is "the host is still alive
// afterwards", so running them against the wrong vendor module proves
// nothing at all.
func IsPVMHost() bool {
	f, err := os.Open("/proc/modules")
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 4095)
	n, _ := f.Read(buf)
	if n <= 0 {
		return false
	}
	return strings.Contains(string(buf[:n]), "kvm_pvm")
}
