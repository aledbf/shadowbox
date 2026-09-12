#!/bin/bash
#
# Runs inside L1, as the PVM host.  Loads kvm-pvm, states plainly whether
# it came up, then boots the guest under it and forwards the guest's
# serial output to L1's console -- which is L0's log file.
#
# Every line it prints is prefixed so the two kernels' output can be told
# apart in a single log.

set -u

# Straight to the console.  Routed through journald, the console output is
# rate limited, and what gets dropped is the end -- which is where the
# diagnostics are.
exec >/dev/console 2>&1

say() { echo "L1: $*"; }

say "kernel: $(uname -r)"
say "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2-)"

# Which vendor to run under.  Both are modules and both travel on the
# payload share, because the rootfs is bootstrapped once and the kernel
# version string -- and so the path modprobe would search -- moves with
# every commit.  insmod by path sidesteps that entirely.
VENDOR="$(sed -n 's/.*pvmtest\.vendor=\([^ ]*\).*/\1/p' /proc/cmdline)"
VENDOR="${VENDOR:-pvm}"
# Read early: the "no /dev/kvm" bail below is a failure for every suite
# except the one whose whole point is that the module refuses.
SUITE="$(sed -n 's/.*pvmtest\.suite=\([^ ]*\).*/\1/p' /proc/cmdline)"
SUITE="${SUITE:-default}"
GUEST_CPUS="$(sed -n 's/.*pvmtest\.guest_cpus=\([^ ]*\).*/\1/p' /proc/cmdline)"
GUEST_CPUS="${GUEST_CPUS:-2}"
GUEST_MEM="$(sed -n 's/.*pvmtest\.guest_mem=\([^ ]*\).*/\1/p' /proc/cmdline)"
GUEST_MEM="${GUEST_MEM:-1G}"
MOD_ARGS="$(sed -n 's/.*pvmtest\.mod_args=\([^ ]*\).*/\1/p' /proc/cmdline | tr , ' ')"
case "$VENDOR" in
pvm)   mod=/mnt/payload/kvm-pvm.ko ;;
intel) mod=/mnt/payload/kvm-intel.ko ;;
*)     say "unknown vendor: $VENDOR"; poweroff -f ;;
esac

say "loading $VENDOR from $mod ${MOD_ARGS:-}"
if ! insmod "$mod" ${MOD_ARGS:-} 2>&1 | sed 's/^/L1: insmod: /'; then
	say "insmod failed"
fi
lsmod | grep -E '^kvm' | sed 's/^/L1: lsmod: /'

dmesg | grep -i -E 'pvm|kvm' | tail -40 | sed 's/^/L1: dmesg: /'

if [ "$SUITE" = failclosed ]; then
	# The module is expected to refuse.  What is checked is that it says
	# why, that it leaves no /dev/kvm behind, and that the host is still
	# healthy -- a refusal that half-registered would be worse than a
	# panic, because nothing would notice.
	say "=== expecting $VENDOR to refuse to load ==="
	if lsmod | grep -q '^kvm_pvm'; then
		echo "FAILCLOSED: fail reason=module-stayed-loaded"
	elif [ -e /dev/kvm ]; then
		echo "FAILCLOSED: fail reason=kvm-device-present"
	else
		echo "FAILCLOSED: ok reason=refused"
	fi
	say "--- what it said ---"
	dmesg | grep -iE 'kvm|pvm' | tail -20 | sed 's/^/L1: dmesg: /'
	poweroff -f
fi

if [ ! -e /dev/kvm ]; then
	say "no /dev/kvm -- the PVM host did not register"
	echo "PVMTEST-RESULT: fail stage=1 reason=no-kvm-device"
	poweroff -f
fi
say "/dev/kvm present"
say "host PTI: $(cat /sys/devices/system/cpu/vulnerabilities/meltdown 2>/dev/null)"
say "host pti flag: $(grep -o ' pti' /proc/cpuinfo | head -1 || echo 'not set')"

# The payload share is already mounted: the stub in the image did it
# before exec'ing this script, which is how this script got here at all.
# Mounting it a second time fails with "no channels available for device
# payload", and this script used to treat that as fatal.

# "profile" runs the guest's perf suite but samples the host's cycles
# rather than counting the guest's exits.  The switcher executes at CPL0
# in the vCPU thread, so it is host kernel text and shows up here.
MODE=run
if [ "$SUITE" = profile ]; then
	MODE=profile
	SUITE=perf
elif [ "$SUITE" = mmu ]; then
	# Counts rather than samples: the shadow MMU tracepoints are far too
	# hot to buffer, and what is wanted is how often each path is taken,
	# not where.
	MODE=mmu
	SUITE=perf
elif [ "$SUITE" = hosttests ]; then
	# No guest at all.  These drive the KVM API from L1 directly, which
	# is the only way to reach the PVM MSRs and the PVCS pinning: the
	# guest-side suite runs at guest CPL3 and cannot touch either.
	MODE=hosttests
fi

if [ "$MODE" = hosttests ]; then
	rc=0
	found=0
	for t in /mnt/payload/hosttests/*; do
		[ -x "$t" ] || continue
		found=1
		say "=== $(basename "$t") ==="
		# Unbuffered through sed so a test that wedges still shows
		# what it managed to print.
		timeout -k 5 300 "$t" 2>&1 | sed "s/^/H: /"
		r=${PIPESTATUS[0]}
		say "$(basename "$t") exited $r"
		[ "$r" = 0 ] || rc=1
	done
	# The kernel's own KVM selftests, if any were staged.  Their verdict
	# is not folded into rc: they build a guest that expects CPL0 and its
	# own page tables, so what they do under PVM is a question rather
	# than an assertion.  The outcome of each is reported by name and
	# classified outside, against configs/kvm-selftests-expect.txt.
	#
	# Exit codes are the kselftest convention: 0 pass, 4 skip, anything
	# else a failure.
	for t in /mnt/payload/kvm-selftests/*; do
		[ -x "$t" ] || continue
		found=1
		name=$(basename "$t" | sed 's/__/\//')
		timeout -k 5 300 "$t" > /tmp/st.log 2>&1
		r=$?
		case $r in
		0)   verdict=pass ;;
		4)   verdict=skip ;;
		124|137) verdict=timeout ;;
		*)   verdict=fail ;;
		esac
		echo "SELFTEST: $name $verdict rc=$r"
		# Only the tail, and only when it did not pass: a passing
		# selftest's output is pages of nothing anyone will read.
		[ "$verdict" = pass ] || tail -15 /tmp/st.log | sed "s|^|S[$name]: |"
	done

	if [ "$found" = 0 ]; then
		say "no host tests in the payload"
		echo "PVMTEST-RESULT: fail stage=hosttests reason=no-tests"
	elif [ "$rc" = 0 ]; then
		echo "PVMTEST-RESULT: ok stage=hosttests"
	else
		echo "PVMTEST-RESULT: fail stage=hosttests"
	fi
	dmesg | tail -60 | sed 's/^/L1: dmesg: /'
	poweroff -f
fi

say "qemu: $(qemu-system-x86_64 -version 2>&1 | head -1)"
ls -l /mnt/payload | sed 's/^/L1: payload: /'

# The guest dies before its first printk, so the only account of what
# happened is the host's.  KVM's tracepoints are the closest thing to a
# debugger here.
T=/sys/kernel/tracing
[ -d "$T" ] || T=/sys/kernel/debug/tracing
if [ -d "$T" ]; then
	echo 0 > "$T/tracing_on" 2>/dev/null
	echo > "$T/trace" 2>/dev/null
	if [ "$MODE" = profile ]; then
		: # no tracepoints while sampling; they cost 20% of the profile
	elif [ "$SUITE" = perf ]; then
		# Just the emulated instructions, with room for a lot of them:
		# the whole kvm event set would overrun the buffer in seconds
		# and the opcode histogram is what the perf suite is for.
		echo 131072 > "$T/buffer_size_kb" 2>/dev/null
		echo 1 > "$T/events/kvm/kvm_emulate_insn/enable" 2>/dev/null &&
			say "tracing emulated instructions"
		echo 1 > "$T/events/kvm/kvm_msr/enable" 2>/dev/null
		# The counters say how many exits there were; nothing says what
		# they were, because a PVM_HC_* exit has no counter of its own
		# -- kvm:kvm_hypercall fires only in kvm_emulate_hypercall(),
		# which PVM reaches for the KVM-specific hypercalls alone.
		# kvm_exit carries the reason, so in mmu mode trace that too.
		if [ "$MODE" = mmu ]; then
			echo 1 > "$T/events/kvm/kvm_exit/enable" 2>/dev/null &&
				say "tracing exit reasons"
		fi
	else
		echo 32768 > "$T/buffer_size_kb" 2>/dev/null
		echo 1 > "$T/events/kvm/enable" 2>/dev/null &&
			say "kvm tracepoints enabled"
	fi
	echo 1 > "$T/tracing_on" 2>/dev/null
fi

case "$SUITE" in
full|perf|all) GUEST_TIMEOUT=1800 ;;
*)          GUEST_TIMEOUT=120 ;;
esac
say "guest: ${GUEST_CPUS} vcpus, ${GUEST_MEM}, timeout ${GUEST_TIMEOUT}s"

APPEND="console=ttyS0,115200 panic=-1 oops=panic pvmtest.suite=$SUITE pvmtest.tag=$VENDOR-guest"
if [ "$SUITE" = perf ]; then
	# A quiet boot for the perf suite.  The serial console is a 16550:
	# every character costs a poll of the line status register and a
	# write to the transmit register, and each of those is a #GP the host
	# emulates.  A verbose boot puts tens of thousands of those into the
	# exit histogram and drowns out what the guest actually does.  The
	# harness still needs the console for its result line, which is
	# forty-odd lines rather than thirty thousand characters.
	APPEND="$APPEND quiet loglevel=0"
	G_EXTRA="$(sed -n 's/.*pvmtest\.guest_append=\([^ ]*\).*/\1/p' /proc/cmdline)"
	[ -n "$G_EXTRA" ] && APPEND="$APPEND $(echo "$G_EXTRA" | tr , ' ')"
	# One benchmark, so the profile is of the thing being asked about.
	PROFILE_CASE="$(sed -n 's/.*pvmtest\.profile_case=\([^ ]*\).*/\1/p' /proc/cmdline)"
	[ "$MODE" = profile ] && APPEND="$APPEND pvmtest.only=${PROFILE_CASE:-perf/syscall}"
else
	APPEND="$APPEND earlyprintk=serial,ttyS0,115200"
fi
# Only a PVM run must have relocated itself; under kvm-intel the same
# image is an ordinary guest and belongs at the usual address.
[ "$VENDOR" = pvm ] && APPEND="$APPEND pvmtest.expect=pvm"

# Two machine types, because they differ in exactly the way that matters.
#
# q35 runs SeaBIOS first, in real mode, and SeaBIOS enables interrupts --
# at which point the host has to inject an IRQ into a guest that is not in
# PVM mode yet.  do_pvm_event() does not support that: it warns and raises
# a triple fault, which is the KVM_EXIT_SHUTDOWN we see.
#
# microvm has no firmware.  The kernel is entered directly, with
# interrupts off, and stays that way until it is in long mode -- so
# nothing is ever injected in non-PVM mode.  If this one gets further,
# that confirms where the problem is.
run_guest() { # $1=tag, rest=machine args
	local tag="$1"; shift
	rm -f /tmp/guest.log /tmp/qemu.log
	say "=== booting the PVM guest on $tag ==="
	# Bounded, so that a guest that wedges its vCPU thread does not take
	# the whole agent down with it and cost us the host side diagnostics.
	# The bound has to fit the suite: the stress and perf cases allow
	# themselves several minutes each, and 45s was chosen back when the
	# guest was dying in under a second.
	timeout -k 5 "$GUEST_TIMEOUT" $PERF_PREFIX qemu-system-x86_64 "$@" \
		-cpu host -smp "$GUEST_CPUS" -m "$GUEST_MEM" \
		-kernel /mnt/payload/guest-vmlinux \
		-initrd /mnt/payload/initrd.cpio.gz \
		-append "$APPEND" \
		-display none -monitor none -serial file:/tmp/guest.log \
		-no-reboot < /dev/null > /tmp/qemu.log 2>&1
	local rc=$?
	say "$tag: qemu exited $rc"
	[ -s /tmp/qemu.log ] && sed "s/^/L1: $tag qemu: /" /tmp/qemu.log
	if [ -s /tmp/guest.log ]; then
		sed "s/^/G[$tag]: /" /tmp/guest.log
	else
		say "$tag: the guest produced no serial output at all"
	fi
}

# Core dumps off, on purpose.  vfs_coredump() is what sleeps, and it is
# sleeping with a leaked preempt count -- so the dump never finishes, qemu
# never exits, and the agent waits on it forever.  Without a dump the
# SIGSEGV just kills qemu and we get to keep debugging.
#
# print-fatal-signals gives the faulting address, RIP and error code for
# the killed process, which is the one line that says whether this is qemu
# faulting or a PVM guest fault reaching the host's own #PF handler.
ulimit -c 0
echo core > /proc/sys/kernel/core_pattern 2>/dev/null
echo 1 > /proc/sys/kernel/print-fatal-signals 2>/dev/null

# For the perf suite, count the guest's exits by reason while it runs.
# This is the whole point of the pvm_trace.h port: "HYPERCALL" and
# "SYSCALL" as bare totals do not say where the time goes.
PERF_PREFIX=""
# The payload carries the libraries L1 itself does not have.
export LD_LIBRARY_PATH=/mnt/payload${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}

if [ "$MODE" = mmu ] && [ -x /mnt/payload/perf ]; then
	cd /tmp || exit 1
	echo -1 > /proc/sys/kernel/perf_event_paranoid
	say "counting exits and shadow MMU events"
	# kvm_exit first, because for a hypervisor it is the number: every
	# other count here is a theory about what those exits were.  The rest
	# split them -- emulated instructions, PIO, MMIO, MSR accesses -- and
	# the kvmmmu ones say how much of it was the shadow MMU.
	#
	# kvm:kvm_hypercall alone is not enough to see PVM hypercalls: it
	# fires in kvm_emulate_hypercall(), which PVM reaches only for the
	# KVM-specific ones.  PVM_HC_* are counted as exits, not as hypercalls.
	PERF_EVENTS="kvm:kvm_exit,kvm:kvm_entry,kvm:kvm_emulate_insn,kvm:kvm_pio,kvm:kvm_mmio,kvm:kvm_msr,kvm:kvm_hypercall"
	PERF_EVENTS="$PERF_EVENTS,kvmmmu:kvm_mmu_get_page,kvmmmu:kvm_mmu_prepare_zap_page"
	PERF_EVENTS="$PERF_EVENTS,kvmmmu:kvm_mmu_sync_page,kvmmmu:kvm_mmu_unsync_page,kvmmmu:fast_page_fault"
	PERF_PREFIX="/mnt/payload/perf stat -a -o /tmp/mmu.txt -e $PERF_EVENTS --"
elif [ "$MODE" = profile ] && [ -x /mnt/payload/perf ]; then
	cd /tmp || exit 1
	echo 0 > /proc/sys/kernel/kptr_restrict
	echo -1 > /proc/sys/kernel/perf_event_paranoid
	# Not system-wide: L1 spends most of its time idle, and "-a" buries
	# the switcher under pv_native_safe_halt.  Following the qemu process
	# still catches the switcher, which runs at CPL0 in the vCPU thread.
	say "sampling qemu's cycles at 4kHz"
	PERF_PREFIX="/mnt/payload/perf record -e cycles:k -F 4000 -g -o /tmp/cycles.data --"
elif [ "$SUITE" = perf ] && [ -x /mnt/payload/perf ]; then
	# "perf kvm stat" is compiled out entirely without libtraceevent, and
	# a perf built that way answers the subcommand with its own usage
	# text -- which, wrapped around qemu, silently costs the whole run.
	# Ask first.
	if /mnt/payload/perf kvm stat record -a -- true >/dev/null 2>&1; then
		say "recording kvm exits with perf"
		# perf writes perf.data.kvm into the current directory, and the
		# unit starts in "/", which is not somewhere to leave files.
		cd /tmp || exit 1
		PERF_PREFIX="/mnt/payload/perf kvm stat record -a --"
	else
		say "perf has no working 'kvm stat' (built without libtraceevent?)"
	fi
fi

run_guest q35 -machine q35,accel=kvm

# Which instructions the host is emulating, and how often.  Every #GP the
# guest takes for a privileged instruction with no paravirt hook lands in
# the emulator, and the exit histogram counts them all as one row.
if { [ "$SUITE" = perf ] || [ "$SUITE" = mmu ]; } && [ -d "$T" ]; then
	# What the exits were.  This is the first question about a hypervisor
	# and the counters cannot answer it: kvm:kvm_hypercall fires only in
	# kvm_emulate_hypercall(), which PVM reaches for the KVM-specific
	# hypercalls alone, so every PVM_HC_* lands here as a bare exit with
	# no counter of its own.  The reason field of kvm_exit has it -- PVM
	# fills it in from pvm_get_syscall_exit_reason() -- so count that.
	# "HYPERCALL" on its own says almost nothing -- a TLB flush and a
	# page-table load cost very different amounts -- so pvm_get_exit_info()
	# puts which one it was in info2.  Substitute it, which is what that
	# field is there for.
	say "--- exits by reason ---"
	sed -n 's/.*kvm_exit: .* reason \(.*\) rip .*info2 \(0x[0-9a-f]*\).*/\1|\2/p' \
		"$T/trace" |
		awk -F'|' '
		BEGIN {
			h["20001"]="HC_IRQ_WIN";    h["20002"]="HC_IRQ_HALT"
			h["20003"]="HC_LOAD_PGTBL"; h["20004"]="HC_TLB_FLUSH"
			h["20005"]="HC_TLB_FLUSH_CURRENT"
			h["20006"]="HC_TLB_INVLPG"
			h["20007"]="HC_LOAD_GS";    h["20008"]="HC_RDMSR"
			h["20009"]="HC_WRMSR";      h["2000a"]="HC_LOAD_TLS"
		}
		{
			r = $1
			if (r == "HYPERCALL") {
				v = $2
				sub(/^0x0*/, "", v)
				r = (v in h) ? h[v] : "HYPERCALL(" v ")"
			}
			n[r]++
		}
		END { for (k in n) printf "%8d  %s\n", n[k], k }' |
		sort -rn | head -18 | sed 's/^/L1: exit: /'

	# Which guest code is causing them.  The reason says what the exit was;
	# this says who asked for it, which is the part you can do something
	# about.  Raw RIPs: the guest kernel is PIE and relocated, so resolving
	# them needs its vmlinux and its runtime _text, both of which live
	# outside L1.  scripts/resolve-exits.sh does that half.
	say "--- exits by reason and guest rip ---"
	sed -n 's/.*kvm_exit: .* reason \(.*\) rip \(0x[0-9a-f]*\).*info2 \(0x[0-9a-f]*\).*/\1|\2|\3/p' \
		"$T/trace" |
		awk -F'|' '
		BEGIN {
			h["20001"]="HC_IRQ_WIN";    h["20002"]="HC_IRQ_HALT"
			h["20003"]="HC_LOAD_PGTBL"; h["20004"]="HC_TLB_FLUSH"
			h["20005"]="HC_TLB_FLUSH_CURRENT"
			h["20006"]="HC_TLB_INVLPG"
			h["20007"]="HC_LOAD_GS";    h["20008"]="HC_RDMSR"
			h["20009"]="HC_WRMSR";      h["2000a"]="HC_LOAD_TLS"
		}
		{
			r = $1
			if (r == "HYPERCALL") {
				v = $3
				sub(/^0x0*/, "", v)
				r = (v in h) ? h[v] : "HYPERCALL(" v ")"
			}
			n[r "|" $2]++
		}
		END { for (k in n) { split(k, p, "|"); printf "%8d  %-22s %s\n", n[k], p[1], p[2] } }' |
		sort -rn | head -25 | sed 's/^/L1: exitrip: /'

	total=$(grep -c kvm_emulate_insn "$T/trace" 2>/dev/null || echo 0)
	say "--- emulated instructions: $total traced ---"
	sed -n 's/.*kvm_emulate_insn: [^:]*:[^:]*:\([0-9a-f ]*\)(.*/\1/p' "$T/trace" |
		awk '{ printf "%s %s %s\n", $1, $2, $3 }' |
		sort | uniq -c | sort -rn | head -20 | sed 's/^/L1: insn: /'
	# The list above is dominated by the bootstrap: SeaBIOS runs fully
	# emulated in non-PVM mode, up to 130 instructions per exit, so a
	# memcpy loop drowns out everything else.  What causes the #GP exits
	# is the privileged subset, so count that separately.
	say "--- privileged instructions only ---"
	sed -n 's/.*kvm_emulate_insn: [^:]*:[^:]*:\([0-9a-f ]*\)(.*/\1/p' "$T/trace" |
		awk '
		{
			# Skip operand-size, address-size and REX prefixes.
			i = 1
			while ($i == "66" || $i == "67" || $i ~ /^4[0-9a-f]$/ ||
			       $i == "f2" || $i == "f3" || $i == "2e" || $i == "3e" ||
			       $i == "26" || $i == "36" || $i == "64" || $i == "65")
				i++
			op = $i
			op2 = $(i+1)
			name = ""
			if (op == "0f") {
				if (op2 == "20") name = "mov %crN,%reg"
				else if (op2 == "22") name = "mov %reg,%crN"
				else if (op2 == "21") name = "mov %drN,%reg"
				else if (op2 == "23") name = "mov %reg,%drN"
				else if (op2 == "30") name = "wrmsr"
				else if (op2 == "32") name = "rdmsr"
				else if (op2 == "31") name = "rdtsc"
				else if (op2 == "a2") name = "cpuid"
				else if (op2 == "01") name = "lgdt/lidt/etc"
				else if (op2 == "06") name = "clts"
				else if (op2 == "09") name = "wbinvd"
				else if (op2 == "00") name = "lldt/ltr/etc"
				else if (op2 == "07") name = "sysret"
				else if (op2 == "05") name = "syscall"
			}
			else if (op == "ec" || op == "ed") name = "in dx"
			else if (op == "ee" || op == "ef") name = "out dx"
			else if (op == "e4" || op == "e5") name = "in imm"
			else if (op == "e6" || op == "e7") name = "out imm"
			else if (op == "6c" || op == "6d") name = "ins"
			else if (op == "6e" || op == "6f") name = "outs"
			else if (op == "fa") name = "cli"
			else if (op == "fb") name = "sti"
			else if (op == "f4") name = "hlt"
			if (name != "") n[name]++
		}
		END { for (k in n) printf "%8d  %s\n", n[k], k }' |
		sort -rn | sed 's/^/L1: priv: /'

	# Which MSRs, since wrmsr is what is left once the console is quiet.
	say "--- MSR accesses by register ---"
	sed -n 's/.*kvm_msr: msr_\([a-z]*\) \([0-9a-f]*\) .*/\1 \2/p' "$T/trace" |
		sort | uniq -c | sort -rn | head -12 | sed 's/^/L1: msr: /'

	say "--- dropped by the trace buffer ---"
	grep -h "overrun" "$T/per_cpu/cpu0/stats" 2>/dev/null | sed 's/^/L1: insn: cpu0 /'
fi

if [ "$MODE" = mmu ] && [ -s /tmp/mmu.txt ]; then
	say "--- shadow MMU event counts ---"
	grep -E "kvmmmu:|kvm:|seconds" /tmp/mmu.txt | sed 's/^/L1: mmu: /'
fi

if [ "$MODE" = profile ] && [ -s /tmp/cycles.data ]; then
	# No --vmlinux: L1 boots with KASLR, so the link-time addresses in the
	# image do not match the running kernel and perf resolves nothing.
	VM=""
	say "--- host cycles, kernel symbols ---"
	/mnt/payload/perf report -i /tmp/cycles.data $VM --stdio --sort symbol \
		--percent-limit 0.4 -g none 2>/dev/null |
		grep -vE "^#|^$" | head -28 | sed 's/^/L1: prof: /'

	say "--- anything with 'switcher' in the name ---"
	/mnt/payload/perf report -i /tmp/cycles.data $VM --stdio --sort symbol \
		-g none 2>/dev/null | grep -i switcher | head -10 | sed 's/^/L1: prof: /'
fi

if [ -n "$PERF_PREFIX" ] && [ "$MODE" != profile ]; then
	# The name depends on whether perf recorded host, guest or both:
	# get_filename_for_perf_kvm() picks between .host, .guest and .kvm.
	for f in /tmp/perf.data.guest /tmp/perf.data.kvm /tmp/perf.data.host; do
		[ -s "$f" ] && data="$f" && break
	done
	if [ -n "${data:-}" ]; then
		say "--- kvm exits by reason ($(basename "$data")) ---"
		/mnt/payload/perf kvm -i "$data" stat report --stdio 2>&1 |
			head -45 | sed 's/^/L1: perf: /'
	else
		say "perf produced no data file"
	fi
fi

if [ -d "$T" ]; then
	echo 0 > "$T/tracing_on" 2>/dev/null
fi

# do_pvm_event() warns once per rate-limit window when the VMM injects an
# event while the vCPU is still in the non-PVM bootstrap mode.  Its
# presence or absence says whether that path was reached at all.
say "--- what the host said about the guest ---"
dmesg | grep -iE "PVM:|non-PVM mode" | tail -6 | sed 's/^/L1: pvm: /' ||
	say "(nothing)"
say "--- injection warnings ---"
if ! dmesg | grep -i "non-PVM mode" | tail -3 | grep . | sed 's/^/L1: warn: /'; then
	say "no 'non-PVM mode' warning"
fi

# The backtrace is the thing.  Print the region around it and nothing
# else: a full dmesg dump is long enough that the tail of it is what gets
# lost, and the tail is the part that matters.
# The most informative line of all, if qemu died: the kernel logs the
# faulting address, instruction pointer and error code for an unhandled
# user signal.  A PVM guest runs at hardware CPL3 inside the qemu thread,
# so a guest fault that reaches the host's own #PF handler looks exactly
# like qemu faulting -- and this line is what tells the two apart.
say "--- did qemu fault, and where ---"
if ! dmesg | grep -E "segfault|trap [a-z]+ ip|traps:" | tail -5 | grep . |
		sed 's/^/L1: sig: /'; then
	say "no segfault reported"
fi

say "--- kernel complaints from the guest run ---"
if ! dmesg | sed -n '/scheduling while atomic\|BUG:\|general protection fault\|unable to handle/,+12p' \
		| head -30 | grep . | sed 's/^/L1: bug: /'; then
	say "no BUG, GPF or fault in dmesg"
fi

# The teardown thread and the mmu-notifier unmap storm are the loudest
# things in the buffer and say nothing.  Everything else, in order, is
# what actually happened -- and picking a thread by name gets it wrong,
# because the qemu IO thread and the vCPU threads share it.
if [ -d "$T" ]; then
	say "--- last 70 trace lines, teardown and unmaps removed ---"
	grep -vE "kvm-nx-lpage|kvm_unmap_hva_range|kvm_hv_stimer_cleanup|kvm-pit|kvm_(pic|ioapic)_set_irq|kvm_set_irq" "$T/trace" |
		tail -70 | sed 's/^/L1: kvm: /'
fi

say "done"
poweroff -f
