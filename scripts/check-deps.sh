#!/usr/bin/env bash
# Says what is missing and how to get it, once, rather than failing part
# way through a kernel build.

source "$(dirname "$0")/lib.sh"

missing=0
have() {
	if command -v "$1" >/dev/null 2>&1; then
		printf '  \033[32mok\033[0m   %-16s %s\n' "$1" "$(command -v "$1")"
	else
		printf '  \033[31mmiss\033[0m %-16s %s\n' "$1" "$2"
		missing=1
	fi
}

echo "build:"
have make    "apt install build-essential"
have gcc     "apt install build-essential"
have bison   "apt install bison"
have flex    "apt install flex"
have bc      "apt install bc"
have cpio    "apt install cpio"
have go      "apt install golang-go, or from go.dev"
have pahole  "apt install dwarves  (optional: BTF)"

echo "run:"
have qemu-system-x86_64 "apt install qemu-system-x86"
have mmdebstrap         "apt install mmdebstrap  (only needed for L1)"
have mkfs.ext4          "apt install e2fsprogs   (only needed for L1)"

# The tooling that turns "it is slow" into "it is slow here".  Missing any
# of these is not fatal, but each one costs a specific capability, so say
# which.
echo "profiling and debugging:"
have_lib() { # header, package, what it buys
	if echo "#include <$1>" | gcc -E - >/dev/null 2>&1; then
		printf '  \033[32mok\033[0m   %-24s %s\n' "$2" "$3"
	else
		printf '  \033[33mmiss\033[0m %-24s %s\n' "$2" "$3"
	fi
}
have_lib traceevent/event-parse.h libtraceevent-dev \
	"REQUIRED for 'perf kvm stat' -- it is compiled out without this"
have_lib elfutils/libdw.h         libdw-dev \
	"DWARF: symbol resolution and --call-graph dwarf"
have_lib libunwind.h              libunwind-dev \
	"call graphs from a profile"
have_lib capstone/capstone.h      libcapstone-dev \
	"perf annotate: which instructions are hot"
have_lib slang.h                  libslang2-dev \
	"perf report TUI"
# python3-config can be installed while the headers it points at are not,
# which perf only discovers most of the way through a build.
# The while loop runs in a subshell, so a plain "exit 0" inside it reports
# the opposite of what it found.  Collect first, then decide.
py_ok=no
for d in $(python3-config --includes 2>/dev/null | tr ' ' '\n' | sed -n 's/^-I//p'); do
	[ -f "$d/Python.h" ] && py_ok=yes
done
if [ "$py_ok" = yes ]; then
	printf '  \033[32mok\033[0m   %-24s %s\n' python3-dev "perf jevents and scripting"
else
	printf '  \033[33mmiss\033[0m %-24s %s\n' python3-dev "perf jevents and scripting"
fi

# Optional: report, but do not fail the check on them.
opt() {
	if command -v "$1" >/dev/null 2>&1; then
		printf '  \033[32mok\033[0m   %-24s %s\n' "$1" "$2"
	else
		printf '  \033[33mmiss\033[0m %-24s %s\n' "$1" "$2"
	fi
}
opt gdb        "attach to a guest kernel with qemu -s -S"
opt sparse     "make C=1 static checking"
opt fakechroot "lets 'make rootfs' run without sudo"

echo "kernel headers and libs:"
for lib in libelf.h openssl/ssl.h; do
	if echo "#include <$lib>" | gcc -E - >/dev/null 2>&1; then
		printf '  \033[32mok\033[0m   %s\n' "$lib"
	else
		printf '  \033[31mmiss\033[0m %-16s apt install libelf-dev libssl-dev\n' "$lib"
		missing=1
	fi
done

echo "host:"
if [ -w /dev/kvm ]; then
	printf '  \033[32mok\033[0m   /dev/kvm writable\n'
else
	printf '  \033[31mmiss\033[0m /dev/kvm  -- sudo usermod -aG kvm %s, then log out and back in\n' "$(id -un)"
	missing=1
fi

# The PVM host refuses to load without these; better to find out now
# than after building two kernels.
for feat in fsgsbase rdtscp cx16; do
	if grep -qw "$feat" /proc/cpuinfo; then
		printf '  \033[32mok\033[0m   cpu has %s\n' "$feat"
	else
		printf '  \033[31mmiss\033[0m cpu lacks %s -- kvm-pvm will refuse to load\n' "$feat"
		missing=1
	fi
done

exit $missing
