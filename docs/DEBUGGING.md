# Getting output out of a guest that dies too early

The guest's own console is the only useful instrument, and the hardest
window to see into is before `setup_arch()` reaches
`parse_early_param()` — because that is where `earlyprintk=` is acted on.
A guest that oopses before then prints into the printk ring buffer and
then reboots, and nothing reaches the serial line.

Every early-boot bug in this port so far has lived in exactly that
window. Two things get you through it.

## The host's account

A guest that never prints anything still leaves a trace on the host.
`scripts/l1-agent.sh` turns on the `kvm` tracepoints around the guest
run and prints the tail afterwards, filtered down to the events that say
something — instruction emulation and the mmu-notifier unmap storm at
qemu exit drown out everything else.

Resolve the RIPs against `out/images/guest-vmlinux.debug`. The trace
prints them at their relocated addresses, and the relocation delta is a
multiple of 2GB, so the low 32 bits are the link-time ones:

```
link_va = 0xffffffff00000000 | (rip & 0xffffffff)
```

`__do_pvm_event()` also prints why it gave up whenever it triple-faults
instead of delivering an event, which is how the PVCS and nested-event
failures were told apart.

## An early console, by hand

When the trace is not enough, give the guest a console before it can
die. `parse_early_options()` is exported to `__init` code, so six lines
in `x86_64_start_kernel()`, right after `pvm_early_setup()`, buy the
whole boot log:

```c
	/* TEMPORARY DEBUG -- not for commit.  parse_args() writes into the
	 * string it is given, so this cannot be a literal. */
	{
		static char dbg_early[] __initdata =
			"earlyprintk=serial,ttyS0,115200";
		parse_early_options(dbg_early);
	}
```

This is what turned "the guest produced no serial output at all" into a
complete oops with a call trace, four times in a row. Take it out again
once the guest gets past `parse_early_param()` on its own.

While hunting an early crash it is also worth dropping `panic=-1` and
`oops=panic` from the guest command line: printk replays its ring buffer
when a console finally registers, so a guest that survives its first
oops may hand you the message for free.
