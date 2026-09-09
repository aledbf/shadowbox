// Package sysinit does the part of PID 1's job that is not testing:
// bring up enough of /proc, /sys and /dev that the tests can look at the
// kernel, and make sure the machine stops afterwards.
package sysinit

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

type mount struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

var mounts = []mount{
	{"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"sysfs", "/sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "mode=0755"},
	{"tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=1777"},
	{"tmpfs", "/run", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=0755"},
}

// Setup mounts the pseudo filesystems and relaxes the two sysctls the
// tests need in order to see anything.  A failure here is reported and
// not fatal: most of the suite still works without, say, /sys, and a
// half-working guest that reports which half is more useful than one
// that dies silently.
func Setup() {
	for _, m := range mounts {
		_ = os.MkdirAll(m.target, 0o755)
		if err := syscall.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
			if err != syscall.EBUSY {
				fmt.Printf("PVMINIT: mount %s on %s: %v\n", m.fstype, m.target, err)
			}
		}
	}

	// /proc/kallsyms is how the tests find out where the kernel actually
	// landed, which is the single most interesting fact about a PVM
	// guest.  Without this it is all zeroes.
	write("/proc/sys/kernel/kptr_restrict", "0")

	// A panic must reach the serial line, not a reboot.
	write("/proc/sys/kernel/panic", "-1")
	write("/proc/sys/kernel/printk", "8 4 1 7")
}

func write(path, value string) {
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		fmt.Printf("PVMINIT: write %s: %v\n", path, err)
	}
}

// Banner prints the handful of facts that make a log readable six weeks
// later: which kernel, where it is mapped, and what it thinks it is
// running on.
func Banner() {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err == nil {
		fmt.Printf("PVMINIT: %s %s %s\n",
			charsToString(u.Sysname[:]), charsToString(u.Release[:]),
			charsToString(u.Version[:]))
	}
	if b, err := os.ReadFile("/proc/cmdline"); err == nil {
		fmt.Printf("PVMINIT: cmdline: %s", b)
	}
	if base, err := KernelTextBase(); err == nil {
		fmt.Printf("PVMINIT: _text at 0x%016x\n", base)
	}
	fmt.Printf("PVMINIT: hypervisor: %s\n", Hypervisor())
}

// KernelTextBase returns the runtime address of _text.  For a PVM guest
// this must be below __START_KERNEL_map (0xffffffff80000000), because the
// hypervisor does not let the guest use the top 2GB -- so this one number
// tells you whether the early relocation ran.
func KernelTextBase() (uint64, error) {
	b, err := os.ReadFile("/proc/kallsyms")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[2] == "_text" {
			var v uint64
			if _, err := fmt.Sscanf(f[0], "%x", &v); err != nil {
				return 0, err
			}
			return v, nil
		}
	}
	return 0, fmt.Errorf("_text not found in /proc/kallsyms")
}

// Hypervisor reports what the kernel decided it is running under.  PVM
// presents the KVM signature, so this alone does not distinguish PVM from
// ordinary KVM; the pvm tests use the kernel text base for that.
func Hypervisor() string {
	b, err := os.ReadFile("/sys/hypervisor/type")
	if err == nil {
		return strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		if strings.Contains(string(b), "hypervisor") {
			return "unnamed (hypervisor flag set)"
		}
	}
	return "none detected"
}

// PowerOff stops the machine.  Called from a defer, so it runs on the
// ordinary path and on a panic in the harness alike -- the outside
// harness reads a result line, and a VM that keeps running after printing
// one is just a three minute wait for the same answer.
func PowerOff() {
	os.Stdout.Sync()
	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	// If that returned, there is nothing left to try.
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	select {}
}

// CmdlineMap is the kernel command line, split into key=value pairs.
type CmdlineMap map[string]string

func Cmdline() CmdlineMap {
	m := CmdlineMap{}
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return m
	}
	for _, tok := range strings.Fields(string(b)) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			v = ""
		}
		m[k] = v
	}
	return m
}

func (m CmdlineMap) Get(key, def string) string {
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return def
}

func charsToString(ca []int8) string {
	b := make([]byte, 0, len(ca))
	for _, c := range ca {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
