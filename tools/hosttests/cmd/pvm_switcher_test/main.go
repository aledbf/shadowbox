// The switcher's direct-switch path, driven from the VMM with a guest that
// does nothing else.
//
// Invariants S1, S2 and S4 in Documentation/virt/kvm/x86/pvm-invariants.rst
// are all properties of the supervisor-to-user direct switch in
// arch/x86/entry/entry_64_switcher.S.  The Coverage section used to call
// them "not checkable from outside", which was wrong: every input that path
// trusts comes from the PVCS, and the PVCS is a guest page.  A guest that
// wants to attack the host writes the hostile value there and issues the
// return-to-user synthetic instruction.  So can we.
//
//	S1  SYSRET with a non-canonical RCX/RIP raises #GP in *kernel* space on
//	    Intel, on a stack the guest controls, which is a host takeover
//	    primitive.  The switcher canonicalises RCX and compares before
//	    committing to sysretq, falling back to IRET.  Case: put a
//	    non-canonical rip in the PVCS and return to user with it.
//
//	S2  EFLAGS entering the guest is masked to SWITCH_ENTER_EFLAGS_ALLOWED
//	    and forced to SWITCH_ENTER_EFLAGS_FIXED.  IOPL, VM, VIF and VIP
//	    must never be settable by the guest.  Case: ask for all of them and
//	    have the guest report the RFLAGS it actually got.
//
//	S4  The fast path does not validate PVCS::user_cs/user_ss; it assumes
//	    them, and is only taken when they are exactly
//	    (__USER_DS << 16) | __USER_CS.  Case: ask for a different pair and
//	    check the guest's architectural CS/SS came from it, which only the
//	    hypervisor's full emulation sets.
//
// What this cannot see from userspace is *which* path serviced a given
// transition: a direct switch produces no KVM exit, and neither does a
// hypervisor-emulated one.  The cases are built so the direct switch is the
// live path -- the guest bounces between its two modes several times first,
// which is what gets both shadow roots into prev_roots[] and clears
// SWITCH_FLAGS_NO_DS_CR3 -- and then assert on the outcome.  A host that
// took the hostile value through the fast path would not be here to print
// the result.
//
// The guest is 30 or so hand-assembled bytes per mode.  It has no BIOS, no
// IDT and no GDT of its own: PVM emulates segmentation, and the VMM hands the
// vCPU a long-mode state directly, which is what
// try_to_convert_to_pvm_mode() turns into a PVM guest.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	. "github.com/aledbf/pvm-testbed/tools/hosttests/internal/harness"
	"github.com/aledbf/pvm-testbed/tools/hosttests/internal/kvm"
	. "github.com/aledbf/pvm-testbed/tools/hosttests/internal/pvm"
)

func init() {
	// Keep main on the process's main thread: every vCPU ioctl comes from
	// one task, as in C, and pvm/pvcs-alias-migrate moves that task.
	runtime.LockOSThread()
}

// --- running it --------------------------------------------------------

type outcome struct {
	reports    int    // user-mode RFLAGS reports
	lastRflags uint32 //
	events     int    // event-handler reports
	lastEvent  uint32 //
	exits      int    // KVM_RUN calls that returned
	stoppedBy  int    // the exit reason that ended the loop, or -1
	err        syscall.Errno
}

// alarm is alarm(2) aimed at the calling thread.  The C program relied on a
// process-directed SIGALRM landing on its only thread; a Go process has
// several, so the signal is sent to the vCPU thread with tgkill, and resent
// until cancelled in case it arrived between two KVM_RUNs.
type alarm struct {
	fired atomic.Bool
	stop  chan struct{}
	done  chan struct{}
}

var alarmSignals = make(chan os.Signal, 1)

func init() {
	// A handler, so that SIGALRM interrupts KVM_RUN instead of killing
	// the process; the notifications themselves are not needed.
	signal.Notify(alarmSignals, syscall.SIGALRM)
	go func() {
		for range alarmSignals {
		}
	}()
}

func startAlarm(seconds int) *alarm {
	a := &alarm{stop: make(chan struct{}), done: make(chan struct{})}
	pid, tid := syscall.Getpid(), syscall.Gettid()

	go func() {
		defer close(a.done)
		t := time.NewTimer(time.Duration(seconds) * time.Second)
		defer t.Stop()
		select {
		case <-a.stop:
			return
		case <-t.C:
		}
		a.fired.Store(true)
		for {
			syscall.Tgkill(pid, tid, syscall.SIGALRM)
			select {
			case <-a.stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	return a
}

func (a *alarm) cancel() {
	close(a.stop)
	<-a.done
}

// guestRun calls KVM_RUN until the guest reports something the caller is
// waiting for, or until it does something else entirely.  A guest that
// neither exits nor faults would otherwise hang the run: the alarm makes
// KVM_RUN return EINTR, which is a failed case rather than a dead test.
func guestRun(v *VM, o *outcome, wantReports int, seconds int) {
	*o = outcome{stoppedBy: -1}

	a := startAlarm(seconds)

	for o.reports < wantReports {
		if _, e := IoctlInt(v.VCPU, kvm.RUN, 0); e != 0 {
			if e == syscall.EINTR && !a.fired.Load() {
				// Not the alarm: a Go runtime signal.
				continue
			}
			o.err = e
			break
		}
		o.exits++

		run := v.RunData()
		if run.ExitReason != kvm.EXIT_MMIO {
			o.stoppedBy = int(run.ExitReason)
			break
		}
		mmio := run.MMIO()
		if mmio.IsWrite == 0 {
			o.stoppedBy = kvm.EXIT_MMIO
			break
		}

		if mmio.PhysAddr == MMIO_REPORT {
			o.lastRflags = binary.LittleEndian.Uint32(mmio.Data[:])
			o.reports++
		} else if mmio.PhysAddr == MMIO_EVENT {
			o.lastEvent = binary.LittleEndian.Uint32(mmio.Data[:])
			o.events++
			break
		} else {
			o.stoppedBy = kvm.EXIT_MMIO
			break
		}
	}

	a.cancel()
}

func describe(o *outcome) string {
	stopped := "nothing"
	if o.stoppedBy >= 0 {
		stopped = ExitReasonName(o.stoppedBy)
	}
	timedOut := ""
	if o.err == syscall.EINTR {
		timedOut = ", TIMED OUT"
	}
	return fmt.Sprintf("%d report(s) (last rflags %s), %d event(s) (last %s), "+
		"%d exit(s), stopped by %s%s",
		o.reports, Hex(o.lastRflags), o.events, Hex(o.lastEvent), o.exits,
		stopped, timedOut)
}

// WARMUP_ROUNDS: bounce between the two modes a few times with everything
// well formed.  Two things come out of it: the harness is proved to work at
// all, and both shadow roots end up in prev_roots[], which is what clears
// SWITCH_FLAGS_NO_DS_CR3 and makes the direct switch the live path for
// everything after.
const WARMUP_ROUNDS = 4

func warmUp(v *VM, g *guest, what string) int {
	var o outcome

	g.ctrlSet(UVA_CODE, 0x202, GOOD_SEL)
	guestRun(v, &o, WARMUP_ROUNDS, 10)
	if o.reports < WARMUP_ROUNDS {
		Nok("%s: the guest never got going: %s", what, describe(&o))
		return -1
	}
	return 0
}

// hostStillAnswering: is the host still there?  Every case here is a
// negative test whose real assertion is this one, and the way to ask is to
// keep using the vCPU.
func hostStillAnswering(v *VM) bool {
	var r kvm.Regs

	_, e := Ioctl(v.VCPU, kvm.GET_REGS, unsafe.Pointer(&r))
	return e == 0
}

func testS1NoncanonicalRIP(v *VM, g *guest) {
	var o outcome

	Case = "pvm/invariant-S1-noncanonical-eretu-rip"
	if warmUp(v, g, "S1") != 0 {
		return
	}

	// The first address above the 47-bit canonical range.  On a 5-level
	// host the switcher canonicalises to 57 bits instead, and this is a
	// perfectly good address there -- so the guest would simply run at it
	// and fault on the unmapped page, which is still not a host takeover.
	// Either way the assertion below holds.
	g.ctrlSet(0x0000800000000000, 0x202, GOOD_SEL)
	guestRun(v, &o, 1, 10)
	desc := describe(&o)

	if o.err == syscall.EINTR {
		Nok("the vCPU never came back: %s", desc)
	} else if !hostStillAnswering(v) {
		Nok("the vCPU stopped answering ioctls: %s", desc)
	} else {
		Ok("%s", desc)
	}
}

func testS2EFLAGS(v *VM, g *guest) {
	var o outcome
	const forbidden = 0x3000 | // IOPL
		0x20000 | // VM
		0x80000 | // VIF
		0x100000 // VIP

	Case = "pvm/invariant-S2-eflags-sanitised"
	if warmUp(v, g, "S2") != 0 {
		return
	}

	g.ctrlSet(UVA_CODE, 0x202|forbidden, GOOD_SEL)
	guestRun(v, &o, 1, 10)
	desc := describe(&o)

	if o.reports < 1 {
		Nok("user mode never reported back: %s", desc)
		return
	}

	got := o.lastRflags
	if got&forbidden != 0 {
		Nok("user mode got RFLAGS %s, which still has %s of "+
			"IOPL/VM/VIF/VIP set", Hex(got), Hex(got&forbidden))
	} else if got&0x2 == 0 || got&0x200 == 0 {
		Nok("user mode got RFLAGS %s, without the fixed bit or IF",
			Hex(got))
	} else {
		Ok("asked for %s, got %s", Hex(uint32(0x202|forbidden)), Hex(got))
	}
}

func testS4Selectors(v *VM, g *guest) {
	var o outcome
	var before, after kvm.Sregs
	oddSel := uint32(HOST_USER_CS) // user_cs kept, user_ss zero

	Case = "pvm/invariant-S4-unexpected-selectors"
	if warmUp(v, g, "S4") != 0 {
		return
	}

	if _, e := Ioctl(v.VCPU, kvm.GET_SREGS, unsafe.Pointer(&before)); e != 0 {
		Nok("KVM_GET_SREGS: %s", Strerror(e))
		return
	}

	// A null SS with the expected CS.  Not the pair the fast path tests
	// for, so it must decline; architecturally fine in long mode, so the
	// hypervisor's emulation can carry it through and the guest keeps
	// running -- which is what makes the two paths tell apart.  The fast
	// path never touches PVM's idea of the guest's CS/SS, so if it had
	// wrongly been taken, SS below would still read as it did before.
	g.ctrlSet(UVA_CODE, 0x202, oddSel)
	guestRun(v, &o, 1, 10)
	desc := describe(&o)

	if !hostStillAnswering(v) {
		Nok("the vCPU stopped answering ioctls: %s", desc)
		return
	}
	if _, e := Ioctl(v.VCPU, kvm.GET_SREGS, unsafe.Pointer(&after)); e != 0 {
		Nok("KVM_GET_SREGS after: %s", Strerror(e))
		return
	}

	if o.reports < 1 {
		// Declining into a guest fault is also a correct answer to an
		// unexpected pair; it is not a correct answer to have run it.
		Ok("the guest did not resume in user mode: %s", desc)
	} else if after.SS.Selector == before.SS.Selector {
		Nok("user mode resumed with SS unchanged (%s): the "+
			"unexpected pair was not emulated, %s",
			Hex(before.SS.Selector), desc)
	} else {
		Ok("SS went %s -> %s through the hypervisor, %s",
			Hex(before.SS.Selector), Hex(after.SS.Selector), desc)
	}
}

// SWITCH_FLAGS_NO_DS_CR3 has to follow the address space the switcher
// loads, in both directions.
//
// Each round supervisor mode loads CTRL_PGD1 and then CTRL_PGD2 and returns
// to user mode on the second.  User mode's SYSCALL back is a direct switch,
// so the first load always finds a table the hypervisor built in user mode
// -- empty -- and exits.  The hypervisor serves it and re-enters in
// supervisor mode on PGD1 with a fresh table, and the second load and the
// return to user are then the switcher's to serve.
//
// With PGD1 the alternate address space, which never runs in user mode and
// so has no user-side root, that re-entry sets NO_DS_CR3.  Loading PGD2
// through the switcher must clear it again, since PGD2's pair is cached; a
// switcher that only ever sets it turns the return to user into one more
// exit per round.  The control run loads the main address space twice and
// has no reason to set the bit at all.  Both runs are the same code with the
// same number of guest instructions, so the difference between them is the
// bug.
const PGTBL_ROUNDS = 200

func pgtblRoundExits(v *VM, exits *uint64) int {
	var o outcome
	var before, after uint64

	if err := v.Stat("exits", &before); err != nil {
		Nok("could not read the vCPU's exits counter: %s", Strerror(err))
		return -1
	}
	guestRun(v, &o, PGTBL_ROUNDS, 30)
	if err := v.Stat("exits", &after); err != nil {
		Nok("could not read the vCPU's exits counter: %s", Strerror(err))
		return -1
	}
	if o.reports < PGTBL_ROUNDS {
		Nok("the guest stopped going round: %s", describe(&o))
		return -1
	}
	*exits = after - before
	return 0
}

func testPgtblNoDSCR3(v *VM, g *guest) {
	mainAS := uint64(GUEST_PHYS_BASE + O_PML4)
	altAS := uint64(GUEST_PHYS_BASE + O_PML4_ALT)
	var control, stale uint64

	Case = "pvm/pgtbl-fastpath-clears-no-ds-cr3"

	g.ctrlSetPgds(mainAS, mainAS)
	if warmUp(v, g, "pgtbl") != 0 {
		return
	}

	if pgtblRoundExits(v, &control) != 0 {
		return
	}

	g.ctrlSetPgds(altAS, mainAS)
	// One round to settle the alternate root into the MMU's cache.
	if warmUp(v, g, "pgtbl") != 0 {
		return
	}
	if pgtblRoundExits(v, &stale) != 0 {
		return
	}

	extra := (float64(stale) - float64(control)) / PGTBL_ROUNDS
	if extra >= 0.5 {
		Nok("%.2f more exits per round through an address space without "+
			"a pair (%d vs %d over %d rounds): returns to user mode "+
			"are exiting after the switcher loaded a paired root",
			extra, stale, control, PGTBL_ROUNDS)
	} else {
		Ok("%d exits with the unpaired detour, %d without, over %d rounds",
			stale, control, PGTBL_ROUNDS)
	}
}

// --- the PVCS alias (host KPTI) and its lifecycle -----------------------

// runCleanRounds runs rounds of well-formed direct switches, every one of
// which must come back as a report with sane RFLAGS and none as an event.
// Returns the vCPU exits the rounds took, or -1 having said why.
func runCleanRounds(v *VM, rounds int, seconds int) int64 {
	var o outcome
	var before, after uint64

	if v.Stat("exits", &before) != nil {
		before = 0
	}
	guestRun(v, &o, rounds, seconds)
	if v.Stat("exits", &after) != nil {
		after = 0
	}
	desc := describe(&o)
	if o.reports < rounds || o.events != 0 {
		Nok("%s", desc)
		return -1
	}
	if o.lastRflags&0x200 == 0 {
		Nok("user mode got RFLAGS %s: %s", Hex(o.lastRflags), desc)
		return -1
	}
	return int64(after - before)
}

// The PVCS moves to another page while the guest runs.
//
// The VMM copies it, points MSR_PVM_VCPU_STRUCT and the guest at the new
// page, and poisons the old one: a return RIP of an unmapped address and an
// ERETU target the guest never uses.  Every direct switch after that reads
// and writes the PVCS through tss_ex.pvcs -- the per-VM alias under host
// KPTI -- so a translation still pointing at the old page sends user mode to
// the poison and the round comes back as an event, not a report.  That is
// the "no stale ASID keeps the old mapping" invariant, observed.
//
// The rounds are also counted: they have to stay direct switches, one exit
// each for the report, or the case would pass by never touching the alias.
const REBIND_ROUNDS = 200

func testPVCSAliasRebind(v *VM, g *guest) {
	kva2 := g.kva + O_PVCS2
	const poison = 0x0000dead00000000

	Case = "pvm/pvcs-alias-rebind"
	if warmUp(v, g, "rebind") != 0 {
		return
	}

	for i := 0; i < 3; i++ {
		from, to, toKVA := uint64(O_PVCS), uint64(O_PVCS2), kva2
		if i%2 != 0 {
			from, to, toKVA = O_PVCS2, O_PVCS, g.kva+O_PVCS
		}

		copy(g.mem[to:to+0x1000], g.mem[from:from+0x1000])
		binary.LittleEndian.PutUint64(g.mem[O_CTRL+CTRL_PVCS:], toKVA)
		if v.SetMSR(MSR_PVM_VCPU_STRUCT, GUEST_PHYS_BASE+to) <= 0 {
			Nok("moving the PVCS to %s was refused", Hex(GUEST_PHYS_BASE+to))
			return
		}
		binary.LittleEndian.PutUint64(g.mem[from+uint64(PVCS_RIP):], poison)

		exits := runCleanRounds(v, REBIND_ROUNDS, 20)
		if exits < 0 {
			return
		}
		if exits > 2*REBIND_ROUNDS {
			Nok("move %d: %d exits over %d rounds -- not direct switches",
				i+1, exits, REBIND_ROUNDS)
			return
		}
	}
	Ok("PVCS moved 3 times, %d clean direct-switch rounds after each", REBIND_ROUNDS)
}

// Direct switches while the vCPU thread is moved across every CPU the
// process may run on, every millisecond.  The alias is per vCPU and needs no
// remapping when the vCPU changes CPU, but the ASID does change, and a PCID
// left over on the CPU it came back to is exactly the stale-translation case
// to rule out.
type migrator struct {
	tid   int
	stop  atomic.Bool
	moves atomic.Uint64
}

// CPU_SETSIZE, and a cpu_set_t of that many bits.
const CPU_SETSIZE = 1024

type cpuSet [CPU_SETSIZE / 64]uint64

func (s *cpuSet) isSet(cpu int) bool { return s[cpu/64]&(1<<(cpu%64)) != 0 }
func (s *cpuSet) set(cpu int)        { s[cpu/64] |= 1 << (cpu % 64) }

func schedGetaffinity(tid int, s *cpuSet) syscall.Errno {
	_, _, e := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY, uintptr(tid),
		unsafe.Sizeof(*s), uintptr(unsafe.Pointer(s)))
	return e
}

func schedSetaffinity(tid int, s *cpuSet) syscall.Errno {
	_, _, e := syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, uintptr(tid),
		unsafe.Sizeof(*s), uintptr(unsafe.Pointer(s)))
	return e
}

func migrateThread(m *migrator, done chan<- struct{}) {
	defer close(done)
	var allowed cpuSet
	cpu := 0

	if schedGetaffinity(m.tid, &allowed) != 0 {
		return
	}
	for !m.stop.Load() {
		for {
			cpu = (cpu + 1) % CPU_SETSIZE
			if allowed.isSet(cpu) {
				break
			}
		}
		var one cpuSet
		one.set(cpu)
		if schedSetaffinity(m.tid, &one) == 0 {
			m.moves.Add(1)
		}
		time.Sleep(time.Millisecond)
	}
	schedSetaffinity(m.tid, &allowed)
}

const MIGRATE_ROUNDS = 20000

func testPVCSAliasMigrate(v *VM, g *guest) {
	m := &migrator{tid: syscall.Gettid()}

	Case = "pvm/pvcs-alias-migrate"
	if warmUp(v, g, "migrate") != 0 {
		return
	}
	done := make(chan struct{})
	go migrateThread(m, done)
	exits := runCleanRounds(v, MIGRATE_ROUNDS, 120)
	m.stop.Store(true)
	<-done
	if exits < 0 {
		return
	}
	if m.moves.Load() < 10 {
		Nok("only %d CPU moves happened; nothing was tested", m.moves.Load())
		return
	}
	Ok("%d rounds, %d exits, across %d CPU moves", MIGRATE_ROUNDS, exits, m.moves.Load())
}

// The alias is PVM_PVCS_ALIAS_BASE + vcpu_idx pages.  Create every vCPU the
// VM may have, give each a PVCS page filled with a pattern, and run only the
// last one: its direct switches write through the highest alias there is.
// Every other vCPU's page must still hold its pattern afterwards, and one
// vCPU more than the maximum must be refused.
const DUMMY_PVCS_BASE = 0x100000 // offset into guest memory: 1MB up

func pattern(i int) uint64 { return 0x5a00000000000000 | uint64(i) }

func testPVCSAliasLastVcpu() {
	var v VM
	var g guest
	var exits int64
	bad := -1

	Case = "pvm/pvcs-alias-last-vcpu"
	v.Setup()
	maxVcpus, _ := IoctlInt(v.KVM, kvm.CHECK_EXTENSION, kvm.CAP_MAX_VCPUS)
	if maxVcpus <= 1 || DUMMY_PVCS_BASE+uint64(maxVcpus)*0x1000 > GUEST_MEM_SIZE {
		Nok("KVM_CAP_MAX_VCPUS is %d, which this case cannot lay out", maxVcpus)
		v.Teardown()
		return
	}

	{
		var rl syscall.Rlimit

		syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl)
		rl.Cur = rl.Max
		syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl)
	}

	fds := make([]int, maxVcpus)

	// The C original's control flow is two labels, swap_back and out;
	// these closures are them.
	out := func() {
		for i := 1; i < maxVcpus; i++ {
			if fds[i] > 0 {
				syscall.Close(fds[i])
			}
		}
		v.Teardown()
	}
	swapBack := func() {
		v.UnmapRun()
		v.MmapRun(fds[0])
		v.VCPU = fds[0]
		out()
	}

	// vCPU 0 is v.VCPU; 1 .. maxVcpus-2 are the dummies.
	for i := 1; i < maxVcpus-1; i++ {
		var e syscall.Errno
		fds[i], e = IoctlInt(v.VM, kvm.CREATE_VCPU, uintptr(i))
		if fds[i] < 0 {
			Nok("creating vCPU %d of %d: %s", i, maxVcpus, Strerror(e))
			out()
			return
		}
	}
	// The last one becomes the vCPU that runs.
	var e syscall.Errno
	fds[maxVcpus-1], e = IoctlInt(v.VM, kvm.CREATE_VCPU, uintptr(maxVcpus-1))
	if fds[maxVcpus-1] < 0 {
		Nok("creating the last vCPU (%d): %s", maxVcpus-1, Strerror(e))
		out()
		return
	}
	if extra, _ := IoctlInt(v.VM, kvm.CREATE_VCPU, uintptr(maxVcpus)); extra >= 0 {
		Nok("vCPU %d of a maximum of %d was created", maxVcpus+1, maxVcpus)
		syscall.Close(extra)
		out()
		return
	}

	v.UnmapRun()
	fds[0] = v.VCPU
	v.VCPU = fds[maxVcpus-1]
	var err error
	if err = v.MmapRun(v.VCPU); err == nil {
		if _, e := Ioctl(v.VCPU, kvm.SET_CPUID2, unsafe.Pointer(v.CPUID)); e != 0 {
			err = e
		}
	}
	if err != nil {
		Nok("setting up the last vCPU: %s", Strerror(err))
		v.VCPU = fds[0]
		out()
		return
	}

	if guestSetup(&v, &g) != 0 {
		Nok("could not build the guest on vCPU %d", maxVcpus-1)
		swapBack()
		return
	}

	for i := 0; i < maxVcpus-1; i++ {
		fd := fds[0]
		if i != 0 {
			fd = fds[i]
		}
		dv := v
		off := uint64(DUMMY_PVCS_BASE) + uint64(i)*0x1000
		pat := pattern(i)

		for j := uint64(0); j < 0x1000; j += 8 {
			binary.LittleEndian.PutUint64(v.Mem[off+j:], pat)
		}
		dv.VCPU = fd
		if dv.SetMSR(MSR_PVM_VCPU_STRUCT, GUEST_PHYS_BASE+off) <= 0 {
			Nok("pinning a PVCS for vCPU %d was refused", i)
			swapBack()
			return
		}
	}

	if warmUp(&v, &g, "last-vcpu") != 0 {
		swapBack()
		return
	}
	exits = runCleanRounds(&v, 200, 20)
	if exits < 0 {
		swapBack()
		return
	}

	for i := 0; i < maxVcpus-1 && bad < 0; i++ {
		off := uint64(DUMMY_PVCS_BASE) + uint64(i)*0x1000
		pat := pattern(i)

		for j := uint64(0); j < 0x1000; j += 8 {
			if binary.LittleEndian.Uint64(v.Mem[off+j:]) != pat {
				bad = i
				break
			}
		}
	}
	if bad >= 0 {
		Nok("vCPU %d's PVCS page was written while only vCPU %d ran", bad, maxVcpus-1)
	} else if exits > 400 {
		Nok("%d exits over 200 rounds on vCPU %d -- not direct switches", exits, maxVcpus-1)
	} else {
		Ok("%d vCPUs; vCPU %d ran 200 direct-switch rounds (%d exits), "+
			"the other %d PVCS pages untouched, vCPU %d refused",
			maxVcpus, maxVcpus-1, exits, maxVcpus-1, maxVcpus+1)
	}

	swapBack()
}

// --- PVM_FEATURE_DIRECT_PF ----------------------------------------------

// The guest's view of the feature: the synthetic CPUID it would check, and
// CR2 as KVM reports it after the guest sets it the PVM way, through
// PVCS::cr2, and exits.
func testDirectPFCPUIDAndCR2Write(v *VM, g *guest) {
	var r kvm.Regs
	var s kvm.Sregs
	var o outcome

	Case = "pvm/direct-pf/cpuid-and-cr2-write"
	if _, e := Ioctl(v.VCPU, kvm.GET_REGS, unsafe.Pointer(&r)); e != 0 {
		Nok("KVM_GET_REGS: %s", Strerror(e))
		return
	}
	r.RIP = g.buildSmodProbeCode()
	if _, e := Ioctl(v.VCPU, kvm.SET_REGS, unsafe.Pointer(&r)); e != 0 {
		Nok("KVM_SET_REGS: %s", Strerror(e))
		return
	}

	guestRun(v, &o, 1, 10)
	desc := describe(&o)
	if o.events != 1 {
		Nok("the probe never reported: %s", desc)
		return
	}
	if o.lastEvent&PVM_FEATURE_DIRECT_PF == 0 {
		Nok("PVM_CPUID_FEATURES.ebx is %s, without PVM_FEATURE_DIRECT_PF",
			Hex(o.lastEvent))
		return
	}
	if _, e := Ioctl(v.VCPU, kvm.GET_SREGS, unsafe.Pointer(&s)); e != 0 {
		Nok("KVM_GET_SREGS: %s", Strerror(e))
		return
	}
	if s.CR2 != CR2_PROBE {
		Nok("guest wrote %s to PVCS::cr2, KVM reports CR2 %s",
			Hex(uint64(CR2_PROBE)), Hex(s.CR2))
	} else {
		Ok("ebx %s; CR2 %s after the guest wrote it", Hex(o.lastEvent), Hex(s.CR2))
	}
}

// A user read of an address with no guest PTE, with the feature enabled
// (@direct) or not.  Either way the guest must see the same fault -- vector
// 14 at the user entry, error code U, PVCS::cr2 the address -- and KVM must
// report that CR2 afterwards.  What differs is who delivered it: pf_guest
// counts the #PFs KVM injected, which a switcher delivery never is.  (Not
// pf_taken: the event handler's own first faults, in supervisor mode, count
// there.)
func testDirectPFFault(v *VM, g *guest, direct bool) {
	wantVA := uint64(FAULT_VA + 0x123)
	var taken0, taken1 uint64
	var s kvm.Sregs
	var o outcome

	if direct {
		Case = "pvm/direct-pf/user-np-delivered-directly"
	} else {
		Case = "pvm/direct-pf/user-np-disabled-slow-path"
	}
	// Warm up first, with the feature off: the warm-up's own first touches
	// of present pages would otherwise be delivered spuriously, to an event
	// handler that only spins.
	if warmUp(v, g, Case) != 0 {
		return
	}
	features := uint64(0)
	if direct {
		features = PVM_FEATURE_DIRECT_PF
	}
	if v.SetMSR(MSR_PVM_FEATURES_ENABLED, features) <= 0 {
		Nok("MSR_PVM_FEATURES_ENABLED write refused")
		return
	}
	if v.Stat("pf_guest", &taken0) != nil {
		Nok("no pf_guest stat")
		return
	}

	g.ctrlSet(UVA_CODE+UCODE_FAULT, 0x202, GOOD_SEL)
	guestRun(v, &o, 1, 10)
	desc := describe(&o)
	v.Stat("pf_guest", &taken1)

	if o.events != 1 || o.lastEvent != (MARK_USER_EVENT|0x100|14) {
		Nok("expected a #PF at the user event entry: %s", desc)
		return
	}
	errcode := binary.LittleEndian.Uint16(g.mem[O_PVCS+PVCS_EVENT_ERRCODE:])
	pvcsCR2 := binary.LittleEndian.Uint64(g.mem[O_PVCS+PVCS_CR2:])
	if _, e := Ioctl(v.VCPU, kvm.GET_SREGS, unsafe.Pointer(&s)); e != 0 {
		Nok("KVM_GET_SREGS: %s", Strerror(e))
		return
	}
	if errcode != 0x4 {
		Nok("error code %s, want U (0x4)", Hex(errcode))
	} else if pvcsCR2 != wantVA {
		Nok("PVCS::cr2 %s, want %s", Hex(pvcsCR2), Hex(wantVA))
	} else if s.CR2 != wantVA {
		Nok("KVM reports CR2 %s after the fault, want %s", Hex(s.CR2), Hex(wantVA))
	} else if direct && taken1 != taken0 {
		Nok("KVM injected the fault (pf_guest %d -> %d): "+
			"not delivered by the switcher", taken0, taken1)
	} else if !direct && taken1 == taken0 {
		Nok("KVM never injected the fault with the feature off")
	} else {
		Ok("#PF U at %s, CR2 synced, pf_guest +%d", Hex(wantVA), taken1-taken0)
	}
}

// The repeat rule, deterministically.  The guest faults on the addresses of
// @pattern in the order given, none of them mapped, and resumes after each;
// pf_taken says how many reached the shadow MMU instead of being delivered
// directly.  The rule fixes that number: never twice in a row on a page, and
// never more than four deliveries in a row.
func testDirectPFRule(v *VM, g *guest, name, pattern string, wantSlow uint64) {
	n := uint64(len(pattern))
	total := n
	var taken0, taken1 uint64
	p := g.mem[O_USTACK+PATTERN_OFF:]
	var o outcome

	Case = name
	if warmUp(v, g, name) != 0 {
		return
	}
	if v.SetMSR(MSR_PVM_FEATURES_ENABLED, PVM_FEATURE_DIRECT_PF) <= 0 {
		Nok("MSR_PVM_FEATURES_ENABLED write refused")
		return
	}

	// Every #PF back to user mode at the pattern routine, via the smod code.
	if v.SetMSR(MSR_PVM_EVENT_ENTRY, g.smodEntry) <= 0 {
		Nok("MSR_PVM_EVENT_ENTRY write refused")
		return
	}
	binary.LittleEndian.PutUint64(p, 0)
	for i := uint64(0); i < n; i++ {
		va := UVA_BASE + 0x10000 + uint64(pattern[i]-'A')*0x1000 + 0x40

		binary.LittleEndian.PutUint64(p[8+i*8:], va)
	}
	binary.LittleEndian.PutUint64(p[8+PATTERN_MAX*8:], total)
	g.ctrlSet(UVA_CODE+UCODE_PATTERN, 0x202, GOOD_SEL)

	if v.Stat("pf_taken", &taken0) != nil {
		Nok("no pf_taken stat")
		return
	}
	guestRun(v, &o, 1, 10)
	v.Stat("pf_taken", &taken1)
	desc := describe(&o)

	if o.reports != 1 || uint64(o.lastRflags) != n {
		Nok("the pattern did not run to the end: %s", desc)
	} else if taken1-taken0 != wantSlow {
		Nok("pattern %s: %d of %d faults reached the shadow MMU, the "+
			"rule says %d", pattern, taken1-taken0, n, wantSlow)
	} else {
		Ok("pattern %s: %d direct, %d through the MMU", pattern,
			n-wantSlow, wantSlow)
	}
}

func main() {
	var v VM
	var g guest

	if !IsPVMHost() {
		Skip()
		os.Exit(0)
	}

	// One VM per case.  These leave the guest in whatever state the
	// hostile value put it in -- faulted, spinning in an event handler, or
	// dead -- and the next case needs a vCPU that starts from the top.
	Case = "pvm/switcher-harness"
	v.Setup()
	if guestSetup(&v, &g) != 0 {
		Nok("could not build the guest; is this a PVM host?")
		v.Teardown()
		fmt.Printf("1..%d\n", Pass()+Fail())
		fmt.Printf("PVMHOSTTEST-RESULT: fail pass=%d fail=%d\n", Pass(), Fail())
		os.Exit(1)
	}
	testS1NoncanonicalRIP(&v, &g)
	v.Teardown()

	v.Setup()
	if guestSetup(&v, &g) == 0 {
		testS2EFLAGS(&v, &g)
	}
	v.Teardown()

	v.Setup()
	if guestSetup(&v, &g) == 0 {
		testS4Selectors(&v, &g)
	}
	v.Teardown()

	v.Setup()
	g.pgtblVariant = true
	if guestSetup(&v, &g) == 0 {
		testPgtblNoDSCR3(&v, &g)
	}
	g.pgtblVariant = false
	v.Teardown()

	v.Setup()
	if guestSetup(&v, &g) == 0 {
		testPVCSAliasRebind(&v, &g)
	}
	v.Teardown()

	v.Setup()
	if guestSetup(&v, &g) == 0 {
		testPVCSAliasMigrate(&v, &g)
	}
	v.Teardown()

	testPVCSAliasLastVcpu()

	v.Setup()
	if guestSetup(&v, &g) == 0 {
		testDirectPFCPUIDAndCR2Write(&v, &g)
	}
	v.Teardown()

	v.Setup()
	if guestSetup(&v, &g) == 0 {
		testDirectPFFault(&v, &g, true)
	}
	v.Teardown()

	v.Setup()
	if guestSetup(&v, &g) == 0 {
		testDirectPFFault(&v, &g, false)
	}
	v.Teardown()

	// AAAAAA: every other one is the same page as the delivery before it.
	// AABCDEFG: the second A resets the run, then four deliveries, F
	// refused, G delivered.  AABCBCBC: B and C alternate, so only the run
	// limit refuses one.
	rules := []struct {
		name, pattern string
		slow          uint64
	}{
		{"pvm/direct-pf/rule-same-page", "AAAAAA", 3},
		{"pvm/direct-pf/rule-run-limit", "AABCDEFG", 2},
		{"pvm/direct-pf/rule-interleaved", "AABCBCBC", 2},
	}

	for _, r := range rules {
		v.Setup()
		if guestSetup(&v, &g) == 0 {
			testDirectPFRule(&v, &g, r.name, r.pattern, r.slow)
		}
		v.Teardown()
	}

	os.Exit(Finish())
}
