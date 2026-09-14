// Host-side tests for the PVM ABI.
//
// Everything the guest-side suite can reach runs at guest CPL3.  The PVM
// MSRs, the PVCS pinning and the memslot lifetime around it are reachable
// only from the VMM, through the KVM API, and nothing tested them.  This is
// the program that does.
//
// The contract being checked is in arch/x86/kvm/pvm/pvm.c:
//
//	MSR_PVM_VCPU_STRUCT  must be page aligned.  An address with no
//	                     memslot behind it is *accepted* on purpose --
//	                     a VMM restoring MSRs before memory regions
//	                     would otherwise fail -- and turned into a
//	                     KVM_REQ_GPC_REFRESH that triple faults the
//	                     guest at entry if it is still bad.  So the
//	                     assertion is that the guest dies and the
//	                     host does not.
//	MSR_PVM_EVENT_ENTRY  must be canonical, and so must +256 and +512.
//
// Every case here is a negative test whose real assertion is "the host is
// still alive afterwards".  A crashed host does not print "not ok"; it prints
// nothing, which is why the harness outside also checks the kernel log.  Run
// it inside L1 under tools/pvmtest (which sanitizes the log), not on its own.
//
// Nothing but the Go standard library and the KVM API: the KVM selftests
// library would pull in a guest ABI that a PVM VM does not have.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	. "github.com/aledbf/pvm-testbed/tools/hosttests/internal/harness"
	"github.com/aledbf/pvm-testbed/tools/hosttests/internal/kvm"
	. "github.com/aledbf/pvm-testbed/tools/hosttests/internal/pvm"
	"github.com/aledbf/pvm-testbed/tools/hosttests/internal/x86"
)

func init() {
	// Keep main on the process's main thread, so that every vCPU ioctl
	// main makes comes from the same task, as it did in C.
	runtime.LockOSThread()
}

// --- MSR_PVM_VCPU_STRUCT, and the memslot under it --------------------

func testVcpuStruct(v *VM) {
	var back uint64

	Case = "pvm/vcpu-struct/unaligned"
	if v.SetMSR(MSR_PVM_VCPU_STRUCT, PVCS_GPA+8) > 0 {
		Nok("an unaligned PVCS address was accepted")
	} else {
		Ok("rejected")
	}

	Case = "pvm/vcpu-struct/valid"
	if v.SetMSR(MSR_PVM_VCPU_STRUCT, PVCS_GPA) <= 0 {
		Nok("a page-aligned PVCS inside a memslot was rejected")
	} else if v.GetMSR(MSR_PVM_VCPU_STRUCT, &back) != 1 || back != PVCS_GPA {
		Nok("read back %s, wrote %s", Hex(back), Hex(uint64(PVCS_GPA)))
	} else {
		Ok("pinned at %s", Hex(uint64(PVCS_GPA)))
	}

	// Deliberately accepted: see the comment on this case in
	// pvm_set_msr().  What must not happen is the host noticing later in
	// a way that takes it down rather than the guest.
	Case = "pvm/vcpu-struct/unbacked-accepted"
	if v.SetMSR(MSR_PVM_VCPU_STRUCT, GUEST_PHYS_BASE+GUEST_MEM_SIZE+0x10000) <= 0 {
		Nok("a PVCS with no memslot was rejected at set time; the " +
			"restore-before-memory-regions path depends on it being " +
			"stored and refreshed later")
	} else {
		Ok("stored, to be resolved by KVM_REQ_GPC_REFRESH")
	}

	Case = "pvm/vcpu-struct/zero-clears"
	if v.SetMSR(MSR_PVM_VCPU_STRUCT, 0) <= 0 {
		Nok("clearing the PVCS was rejected")
	} else {
		Ok("cleared")
	}

	// Leave it valid for the churn test.
	v.SetMSR(MSR_PVM_VCPU_STRUCT, PVCS_GPA)
}

// --- MSR_PVM_FEATURES_ENABLED ------------------------------------------

// Support is the hypervisor's to state and acceptance the guest's: the MSR
// starts at zero whatever PVM_CPUID_FEATURES says, takes exactly the
// advertised bits, and refuses the rest.  Run on a vCPU nothing has written
// yet, which is the only way to see the reset value.
func testFeaturesEnabled(v *VM) {
	back := ^uint64(0)

	Case = "pvm/features-enabled/reset-zero"
	if v.GetMSR(MSR_PVM_FEATURES_ENABLED, &back) != 1 {
		Nok("MSR_PVM_FEATURES_ENABLED cannot be read")
	} else if back != 0 {
		Nok("a new vCPU has features enabled: %s", Hex(back))
	} else {
		Ok("zero")
	}

	Case = "pvm/features-enabled/direct-pf"
	if v.SetMSR(MSR_PVM_FEATURES_ENABLED, PVM_FEATURE_DIRECT_PF) <= 0 {
		Nok("enabling PVM_FEATURE_DIRECT_PF was refused")
	} else if v.GetMSR(MSR_PVM_FEATURES_ENABLED, &back) != 1 ||
		back != PVM_FEATURE_DIRECT_PF {
		Nok("read back %s", Hex(back))
	} else {
		Ok("enabled")
	}

	Case = "pvm/features-enabled/unsupported-refused"
	if v.SetMSR(MSR_PVM_FEATURES_ENABLED, PVM_FEATURE_DIRECT_PF<<1) > 0 {
		Nok("a feature bit the hypervisor does not advertise was accepted")
	} else if v.GetMSR(MSR_PVM_FEATURES_ENABLED, &back) != 1 ||
		back != PVM_FEATURE_DIRECT_PF {
		Nok("the refused write changed the MSR to %s", Hex(back))
	} else {
		Ok("refused, value kept")
	}

	Case = "pvm/features-enabled/disable"
	if v.SetMSR(MSR_PVM_FEATURES_ENABLED, 0) <= 0 {
		Nok("writing zero was refused")
	} else if v.GetMSR(MSR_PVM_FEATURES_ENABLED, &back) != 1 || back != 0 {
		Nok("read back %s after writing zero", Hex(back))
	} else {
		Ok("disabled")
	}
}

// --- MSR_PVM_EVENT_ENTRY ----------------------------------------------

// host5Level says whether the host runs 5-level paging: the kernel reports
// la57 only then.
//
// Read the way fgets() into a 4096-byte buffer reads it, so that a flags
// line too long for the buffer behaves as it did in C.
func host5Level() bool {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := fgets(r, 4096)
		if line == "" && err != nil {
			return false
		}
		if strings.HasPrefix(line, "flags") && strings.Contains(line, " la57") {
			return true
		}
	}
}

// fgets reads at most @size-1 bytes, stopping after a newline.
func fgets(r *bufio.Reader, size int) (string, error) {
	var sb strings.Builder
	for sb.Len() < size-1 {
		c, err := r.ReadByte()
		if err != nil {
			return sb.String(), err
		}
		sb.WriteByte(c)
		if c == '\n' {
			break
		}
	}
	return sb.String(), nil
}

func testEventEntry(v *VM) {
	// The entry has to be canonical at +0, +256 and +512, because the
	// three entry points live at those offsets.  An address just below
	// the canonical hole is canonical itself and not canonical at +256,
	// which is the case a single check would miss.
	//
	// The top of the canonical lower half is the host's: bit 47 on a
	// 4-level host, bit 56 on a 5-level one, where every value below
	// would be a perfectly canonical address.
	top := uint64(1) << 47
	if host5Level() {
		top = uint64(1) << 56
	}
	bad := []struct {
		why string
		val uint64
	}{
		{"non-canonical", top},
		{"canonical, +256 is not", top - 0x10},
		{"canonical, +512 is not", top - 0x1f0},
	}

	for i, b := range bad {
		Case = fmt.Sprintf("pvm/event-entry/reject-%d", i)
		if v.SetMSR(MSR_PVM_EVENT_ENTRY, b.val) > 0 {
			Nok("%s (%s) was accepted", Hex(b.val), b.why)
		} else {
			Ok("%s rejected", b.why)
		}
	}
}

// --- the whole PVM MSR window -----------------------------------------

// pvmMSRRange is how many MSR numbers the ABI sets aside from
// PVM_VIRTUAL_MSR_BASE, defined and reserved together.
const pvmMSRRange = 16

func testMSRWindow(v *VM) {
	values := []uint64{0, 1, ^uint64(0), 0x0000800000000000, 0xffffffff80000000}
	survived := true

	// Every MSR in the 16-number PVM range, the reserved tail included,
	// with values chosen to be wrong.  Nothing
	// here asserts an outcome -- some of these are legitimately accepted
	// -- only that the host is still answering ioctls afterwards.
	Case = "pvm/msr-window/sweep"
	for msr := uint32(PVM_VIRTUAL_MSR_BASE); msr < PVM_VIRTUAL_MSR_BASE+pvmMSRRange; msr++ {
		for _, val := range values {
			if v.SetMSR(msr, val) == -int(syscall.EINVAL) {
				survived = false
				Nok("KVM_SET_MSRS(%s) failed with EINVAL, "+
					"which is the ioctl refusing the request "+
					"rather than the vendor refusing the value",
					Hex(msr))
				break
			}
		}
	}
	if survived {
		var dummy uint64

		if v.GetMSR(MSR_PVM_VCPU_STRUCT, &dummy) < 0 {
			Nok("the vCPU stopped answering after the sweep")
		} else {
			Ok("%d MSRs x %d values, host still answering",
				pvmMSRRange, len(values))
		}
	}

	// Put the PVCS back so later cases start from a known state.
	v.SetMSR(MSR_PVM_VCPU_STRUCT, 0)
}

// --- memslot churn under a pinned PVCS --------------------------------

// Shared with the vCPU goroutine.  Atomics, because the churn loop spins on
// runEntered waiting for the goroutine to be under way.
var (
	churnStop                   atomic.Bool
	runEntered, runFailed       atomic.Int64
	runLastErrno, runLastReason atomic.Int64
	runLastSuberror             atomic.Uint32
	vcpuThreadDone              chan struct{}
)

func vcpuThread(v *VM) {
	defer close(vcpuThreadDone)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// The vCPU has no valid PVM state, so every entry ends badly -- a
	// triple fault, or an ioctl error.  That is fine and is not what is
	// being tested: the point is to have entries in flight while the
	// memory map changes under the pinned PVCS, so that
	// pvm_vcpu_gpc_refresh() runs against a moving target.
	//
	// Which is exactly why the outcome is counted.  A KVM_RUN that is
	// refused by the ioctl before the vCPU is ever entered would make
	// this test pass without churning anything, and the pass would look
	// identical.
	for i := 0; i < 100000 && !churnStop.Load(); i++ {
		if r, e := KVMRun(v.VCPU); r < 0 {
			runFailed.Add(1)
			runLastErrno.Store(int64(e))
		} else {
			runEntered.Add(1)
			run := v.RunData()
			runLastReason.Store(int64(run.ExitReason))
			if run.ExitReason == kvm.EXIT_INTERNAL_ERROR {
				runLastSuberror.Store(run.Internal().Suberror)
			}
		}
	}
}

func testMemslotChurn(v *VM) {
	var i int
	var err syscall.Errno

	Case = "pvm/memslot-churn"

	if v.SetMSR(MSR_PVM_VCPU_STRUCT, PVCS_GPA) <= 0 {
		Nok("could not pin a PVCS to churn under")
		return
	}

	churnStop.Store(false)
	runEntered.Store(0)
	runFailed.Store(0)
	vcpuThreadDone = make(chan struct{})
	go vcpuThread(v)

	// Wait for the vCPU thread to actually be entering before touching
	// the memory map.  The churn is 200 ioctls and finishes in well under
	// a scheduling quantum, so without this the whole loop can run and
	// set churnStop before the thread is first scheduled -- and then the
	// case passes having tested nothing.  That is not hypothetical; it is
	// what this test did until it was made to count its own entries.
	for i = 0; i < 100000 && runEntered.Load() == 0 && runFailed.Load() == 0; i++ {
		time.Sleep(100 * time.Microsecond)
	}
	if runEntered.Load() == 0 && runFailed.Load() == 0 {
		churnStop.Store(true)
		<-vcpuThreadDone
		Nok("the vCPU thread never made a KVM_RUN call")
		return
	}

	// Delete and re-add the slot the PVCS lives in, then move it, while
	// the vCPU thread is entering.  Deleting it unpins the PVCS page from
	// under a vCPU that may be about to use it.
	mem := v.MemAddr()
	for i = 0; i < 50; i++ {
		if r := v.SetMemslot(0, GUEST_PHYS_BASE, 0, mem, 0); r != 0 {
			err = syscall.Errno(-r)
			break
		}
		if r := v.SetMemslot(0, GUEST_PHYS_BASE, GUEST_MEM_SIZE, mem, 0); r != 0 {
			err = syscall.Errno(-r)
			break
		}
		// Move it somewhere else and back.
		if r := v.SetMemslot(0, GUEST_PHYS_BASE, 0, mem, 0); r != 0 {
			err = syscall.Errno(-r)
			break
		}
		if r := v.SetMemslot(0, GUEST_PHYS_BASE+GUEST_MEM_SIZE, GUEST_MEM_SIZE, mem, 0); r != 0 {
			err = syscall.Errno(-r)
			break
		}
		if r := v.SetMemslot(0, GUEST_PHYS_BASE+GUEST_MEM_SIZE, 0, mem, 0); r != 0 {
			err = syscall.Errno(-r)
			break
		}
		if r := v.SetMemslot(0, GUEST_PHYS_BASE, GUEST_MEM_SIZE, mem, 0); r != 0 {
			err = syscall.Errno(-r)
			break
		}
	}

	churnStop.Store(true)
	<-vcpuThreadDone

	if err != 0 {
		Nok("memslot update failed after %d rounds: %s", i, Strerror(err))
		return
	}

	// Still alive, still answering.
	if v.SetMSR(MSR_PVM_VCPU_STRUCT, PVCS_GPA) <= 0 {
		Nok("the vCPU stopped accepting a PVCS after the churn")
		return
	}

	entered, failed := runEntered.Load(), runFailed.Load()
	lastErrno, lastReason := runLastErrno.Load(), int(runLastReason.Load())

	// The churn is only a test of the refresh path if the vCPU actually
	// entered.  If every KVM_RUN was refused outright then nothing was in
	// flight and this case proves nothing -- say so rather than printing
	// a green line.
	if entered == 0 {
		Nok("%d rounds, but not one KVM_RUN entered the vCPU "+
			"(%d refused, last errno %d): nothing was in flight, so "+
			"this case tested nothing",
			i, failed, lastErrno)
		return
	}

	// A vCPU that was never given valid PVM state cannot get far, so an
	// unhappy exit is the expected outcome and is not the assertion.  It
	// is reported by name because "17" in a log six months from now is
	// not information, and because an internal error's suberror says
	// whether KVM gave up in the emulator or somewhere less ordinary.
	if lastReason == kvm.EXIT_INTERNAL_ERROR {
		Ok("%d rounds; %d entries (last exit %s/%d), %d refused",
			i, entered, ExitReasonName(lastReason),
			runLastSuberror.Load(), failed)
	} else {
		Ok("%d rounds; %d entries (last exit %s), %d refused "+
			"(last errno %d)", i, entered,
			ExitReasonName(lastReason), failed, lastErrno)
	}
}

// --- a PVCS the host cannot pin, taken all the way to KVM_RUN ---------

// virt-pvm/linux#7: a cross-host snapshot restore panicked the *host* with a
// NULL dereference in pvm_vcpu_run(), reading pvcs->event_flags with the
// guard on pvm->msr_vcpu_struct -- the guest physical address -- rather than
// on pvm->pvcs, the mapped pointer.  A VMM that restores MSRs before it adds
// memory regions sets one without the other.
//
// The shape that crashed is still in the code, deliberately: the fix is the
// WARN_ON_ONCE plus triple fault at the top of pvm_vcpu_run(), and before
// that the KVM_REQ_GPC_REFRESH that pvm_set_msr() raises when the pin fails.
// What these two cases check is that the fail-closed path really is the one
// taken, from the VMM's side, for both ways of making the pin fail: no
// memslot at all, and a memslot whose backing memory cannot be pinned for
// write.
//
// The assertion in both is the same and it is about the host: the ioctls
// keep working afterwards.  A host that oopsed would not answer at all.
func runWithUnpinnablePVCS(v *VM, what string, gpa uint64) {
	entered, refused, lastErrno, lastReason := 0, 0, 0, -1

	if v.SetMSR(MSR_PVM_VCPU_STRUCT, gpa) <= 0 {
		Nok("%s: setting the PVCS was rejected outright; the "+
			"restore-before-memory-regions path depends on it being "+
			"stored and resolved later", what)
		return
	}

	for i := 0; i < 20; i++ {
		if r, e := KVMRun(v.VCPU); r < 0 {
			refused++
			lastErrno = int(e)
		} else {
			entered++
			lastReason = int(v.RunData().ExitReason)
		}
	}

	// Back to something sane, and prove the vCPU still answers.
	if v.SetMSR(MSR_PVM_VCPU_STRUCT, 0) <= 0 {
		Nok("%s: the vCPU stopped answering after %d entries",
			what, entered)
		return
	}

	if entered == 0 && refused == 0 {
		Nok("%s: KVM_RUN was never called", what)
		return
	}

	last := "none"
	if lastReason >= 0 {
		last = ExitReasonName(lastReason)
	}
	Ok("%s: %d entries (last exit %s), %d refused (last errno %d), "+
		"host still answering", what, entered, last, refused, lastErrno)
}

func testUnpinnablePVCS(v *VM) {
	Case = "pvm/vcpu-struct/unbacked-run"
	runWithUnpinnablePVCS(v, "no memslot behind the PVCS",
		GUEST_PHYS_BASE+GUEST_MEM_SIZE+0x10000)

	// A memslot whose userspace mapping is read-only.  PVM pins the PVCS
	// page with FOLL_WRITE -- it writes the event frame into it -- so the
	// pin fails even though the memslot itself is perfectly valid.
	Case = "pvm/vcpu-struct/readonly-run"
	ro, err := syscall.Mmap(-1, 0, GUEST_MEM_SIZE, syscall.PROT_READ,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS|syscall.MAP_NORESERVE)
	if err != nil {
		Nok("mmap PROT_READ: %s", Strerror(err))
		return
	}
	roAddr := uintptr(unsafe.Pointer(&ro[0]))
	if r := v.SetMemslot(1, GUEST_PHYS_BASE+2*GUEST_MEM_SIZE, GUEST_MEM_SIZE, roAddr, 0); r != 0 {
		Nok("adding a read-only-backed memslot: %s", Strerror(syscall.Errno(-r)))
		syscall.Munmap(ro)
		return
	}
	runWithUnpinnablePVCS(v, "PVCS on read-only backing memory",
		GUEST_PHYS_BASE+2*GUEST_MEM_SIZE)
	v.SetMemslot(1, GUEST_PHYS_BASE+2*GUEST_MEM_SIZE, 0, roAddr, 0)
	syscall.Munmap(ro)
}

// --- does the host's PKRU leak into guest state? ----------------------

// PVM runs the guest at hardware CPL3 on the host's own PKRU, so
// pvm_load_guest_xsave_state() forces PKRU to 0 before entry -- otherwise a
// host process with a restrictive PKRU would deny the guest access to its
// own pages -- and restores the host's value on exit.
//
// The trouble is the order.  Common KVM brackets ->vcpu_run() with
// kvm_load_guest_pkru() / kvm_load_host_pkru(), and the latter does
//
//	vcpu->arch.pkru = rdpkru();
//
// which runs *after* PVM's wrapper has already put the host's value back.
// So vcpu->arch.pkru -- the guest's architectural PKRU, what KVM_GET_XSAVE
// hands the VMM and what goes into a snapshot -- ends up holding the host's.
//
// Both halves of the invariant are at stake: the host's PKRU must not
// restrict the guest (the wrapper sees to that) and must not leak into it
// (this is where it does).  The bracketing only runs when the guest has
// XCR0.PKRU or CR4.PKE set, so the test sets CR4.PKE the way a Linux guest
// with PKU advertised would.
func hostPKRU() uint32 {
	return x86.RDPKRU()
}

const XFEATURE_PKRU = 9

// The same rule the SPEC_CTRL family is held to: a feature may only be
// advertised if the state behind it is actually handled.  PVM runs the guest
// at hardware CPL3 on the host's PKRU and forces it to zero for the
// duration, so there is no guest architectural PKRU at all -- one register
// is being asked to be three things at once (the host's, the guest's, and
// PVM's own supervisor isolation).

// hostHasOSPKE: does this CPU have OSPKE, i.e. is there any PKU for KVM to
// pass through?
func hostHasOSPKE() bool {
	return x86.CPUIDCount(7, 0)[2]&(1<<4) != 0
}

func testPKUNotAdvertised(v *VM) {
	pku := CPUIDHas(v.CPUID, 7, 0, 2, 3)
	ospke := CPUIDHas(v.CPUID, 7, 0, 2, 4)
	haveOSPKE := hostHasOSPKE()

	// PKU is advertised, and has to be: the guest runs on the shadow page
	// tables at CPL 3 on the real PKRU, so the hardware enforces its keys.
	// Upstream KVM clears PKU whenever !tdp_enabled, which PVM undoes
	// deliberately -- if that ever stops happening the guest silently
	// loses protection keys, and nothing else here would notice.
	//
	// OSPKE follows the guest's own CR4.PKE, so it is absent from
	// KVM_GET_SUPPORTED_CPUID; OSPKE without PKU would mean the two had
	// come apart, telling a guest about keys it cannot reach.
	Case = "pvm/pku-advertised"
	if !haveOSPKE {
		Ok("the host has no OSPKE, so there is no PKU to advertise")
	} else if ospke && !pku {
		Nok("OSPKE is advertised without PKU; a guest cannot set " +
			"CR4.PKE and so cannot reach the keys it is told about")
	} else if !pku {
		Nok("KVM_GET_SUPPORTED_CPUID does not advertise PKU on a host " +
			"that has OSPKE; the guest gets no protection keys")
	} else {
		Ok("PKU advertised; security/pkey-denied is what says it works")
	}
}

func testPKRULeak(v *VM) {
	var sregs kvm.Sregs
	var xsave kvm.XSave

	Case = "pvm/pkru-not-leaked"

	r := x86.CPUIDCount(0, 0)
	if r[0] < 0xd {
		Ok("no XSAVE leaf on this CPU; nothing to check")
		return
	}
	r = x86.CPUIDCount(0xd, 0)
	if r[0]&(1<<XFEATURE_PKRU) == 0 {
		Ok("the host CPU has no PKRU xstate component")
		return
	}
	r = x86.CPUIDCount(0xd, XFEATURE_PKRU)
	size := r[0]
	offset := r[1]
	if size < 4 || uint64(offset+size) > uint64(unsafe.Sizeof(xsave.Region)) {
		Nok("PKRU xstate component is at offset %d size %d, which does "+
			"not fit the KVM_GET_XSAVE buffer", offset, size)
		return
	}

	// A Linux guest told it has PKU sets CR4.PKE; do the same.
	if _, e := Ioctl(v.VCPU, kvm.GET_SREGS, unsafe.Pointer(&sregs)); e != 0 {
		Nok("KVM_GET_SREGS: %s", Strerror(e))
		return
	}
	sregs.CR4 |= X86_CR4_PKE
	if _, e := Ioctl(v.VCPU, kvm.SET_SREGS, unsafe.Pointer(&sregs)); e != 0 {
		Ok("the host refuses CR4.PKE (%s), so the PKRU bracketing "+
			"never runs", Strerror(e))
		return
	}

	// And confirm it stuck.  kvm_load_{guest,host}_pkru() only run when
	// the guest has XCR0.PKRU or CR4.PKE, so a CR4 write that was quietly
	// dropped would make this case pass while testing nothing -- the same
	// trap the memslot churn case fell into.
	sregs = kvm.Sregs{}
	if _, e := Ioctl(v.VCPU, kvm.GET_SREGS, unsafe.Pointer(&sregs)); e != 0 {
		Nok("KVM_GET_SREGS after setting CR4.PKE: %s", Strerror(e))
		return
	}
	if sregs.CR4&X86_CR4_PKE == 0 {
		Nok("CR4.PKE did not stick (cr4=%s), so the PKRU bracketing "+
			"never ran and this case proved nothing", Hex(sregs.CR4))
		return
	}

	for i := 0; i < 5; i++ {
		KVMRun(v.VCPU)
	}

	xsave = kvm.XSave{}
	if _, e := Ioctl(v.VCPU, kvm.GET_XSAVE, unsafe.Pointer(&xsave)); e != 0 {
		Nok("KVM_GET_XSAVE: %s", Strerror(e))
		return
	}
	region := unsafe.Slice((*byte)(unsafe.Pointer(&xsave.Region)), unsafe.Sizeof(xsave.Region))
	guestPKRU := binary.LittleEndian.Uint32(region[offset:])
	mine := hostPKRU()

	if guestPKRU != 0 && guestPKRU == mine {
		Nok("the guest's saved PKRU is %s, which is this process's own "+
			"PKRU: the host's value is being handed to the VMM as guest "+
			"state and would be written into a snapshot", Hex(guestPKRU))
		return
	}
	// A clean result here is worth less than it looks.  This vCPU has no
	// PVM state, so pvm_vcpu_run() returns early on non_pvm_mode and the
	// PVM wrappers that put the host's PKRU back never run -- which is
	// exactly the step that would make the core's
	// "vcpu->arch.pkru = rdpkru()" pick up the host's value.  So this
	// shows the core's own bracketing round-trips correctly; it does not
	// clear PVM of the leak, which needs a vCPU that really enters.
	Ok("guest PKRU %s, host PKRU %s (vCPU never left non-PVM mode, so "+
		"the PVM wrappers did not run)", Hex(guestPKRU), Hex(mine))
}

// --- the CPL3 invariants, checked from the VMM's side ------------------

// These reference Documentation/virt/kvm/x86/pvm-invariants.rst by name.
// The point of doing it here rather than as free-standing checks is that the
// document is the contract: if an invariant is restated, the test that
// carries its number is the one that has to change with it.
func testInvariants(v *VM) {
	// M4: a guest kernel page and a guest user page are both USER to the
	// hardware, so the guest-kernel-executes-guest-user case is blocked by
	// setting NX on the user mapping.  With no host NX there is no
	// substitute, and the document says such a host must be rejected at
	// module load rather than silently degraded.  So SMEP may only be
	// advertised where NX exists -- if it were advertised without, the
	// guest would be told it has a protection nothing implements.
	Case = "pvm/invariant-M4-smep-needs-nx"
	r := x86.CPUIDCount(0x80000001, 0)
	hostNX := r[3]&(1<<20) != 0
	if CPUIDHas(v.CPUID, 7, 0, 1, 7) && !hostNX {
		Nok("SMEP is advertised to the guest but the host has no NX; " +
			"invariant M4 has nothing to emulate it with")
	} else {
		Ok("SMEP advertised=%d, host NX=%d",
			B2I(CPUIDHas(v.CPUID, 7, 0, 1, 7)), B2I(hostNX))
	}

	// M4's other half, and the reason SMAP is not in the same sentence:
	// there is no equivalent trick for supervisor-mode *access*
	// prevention, so SMAP must not be advertised at all.
	Case = "pvm/invariant-M4-no-smap"
	if CPUIDHas(v.CPUID, 7, 0, 1, 20) {
		Nok("SMAP is advertised, but PVM emulates no equivalent: the " +
			"guest runs at CPL3 and hardware SMAP cannot separate its " +
			"two modes")
	} else {
		Ok("SMAP not advertised")
	}

	// M5: host and guest share a hardware CR3, so the shadow tree has the
	// host's depth.  A guest may only be offered LA57 where the host has
	// it.
	Case = "pvm/invariant-M5-la57-matches-host"
	r = x86.CPUIDCount(7, 0)
	hostLA57 := r[2]&(1<<16) != 0
	if CPUIDHas(v.CPUID, 7, 0, 2, 16) && !hostLA57 {
		Nok("LA57 is advertised to the guest but the host is 4-level; " +
			"the shadow root level cannot match the host's")
	} else {
		Ok("LA57 advertised=%d, host LA57=%d",
			B2I(CPUIDHas(v.CPUID, 7, 0, 2, 16)), B2I(hostLA57))
	}

	// The host PKRU half of the CPL3 argument: the guest runs at CPL3 on
	// the host's PKRU, so pvm_load_guest_xsave_state() forces it to 0
	// before entry -- otherwise a host process with a restrictive PKRU
	// would deny the guest access to its own pages.
	//
	// That path is only exercised when the host PKRU is actually
	// restrictive.  On a normal Linux host it is init_pkru (0x55555554),
	// which is why every guest run in this testbed already exercises it
	// and they all work.  This case exists to notice if that stops being
	// true, because then the coverage claim would be vacuous rather than
	// satisfied.
	Case = "pvm/invariant-host-pkru-is-restrictive"
	r = x86.CPUIDCount(7, 0)
	if r[2]&(1<<4) == 0 {
		Ok("no OSPKE on the host; nothing to restrict with")
	} else if hostPKRU() == 0 {
		Nok("this process's PKRU is 0, so every guest run has been " +
			"exercising the easy case: the wrapper that forces PKRU " +
			"to 0 for the guest is never doing anything")
	} else {
		Ok("host PKRU is %s, so the forced-to-zero path is live",
			Hex(hostPKRU()))
	}
}

func main() {
	var v VM

	if !IsPVMHost() {
		Skip()
		os.Exit(0)
	}

	v.Setup()

	testFeaturesEnabled(&v)
	testVcpuStruct(&v)
	testEventEntry(&v)
	testMSRWindow(&v)
	testUnpinnablePVCS(&v)
	testPKUNotAdvertised(&v)
	testInvariants(&v)
	testPKRULeak(&v)
	testMemslotChurn(&v)

	v.Teardown()

	os.Exit(Finish())
}
