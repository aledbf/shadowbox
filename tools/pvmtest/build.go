package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// BuildKernel builds one of the two test kernels out of tree, from defconfig
// plus the testbed's fragments, into c.Out/build-<role>, and installs the
// images (and for the host, its modules) under c.Out.
//
// extra are CONFIG_FOO=y|m|n settings on top ("CONFIG_FOO=n" is written back
// by kconfig as "# CONFIG_FOO is not set", and checked that way).  A tree
// without PVM -- upstream, for the KVM side of a comparison -- gets the same
// fragments with the PVM options taken out, and is not required to have
// them.
func BuildKernel(c Config, role string, extra []string) error {
	if err := c.NeedKSRC(); err != nil {
		return err
	}
	for _, t := range []struct{ tool, hint string }{
		{"make", ""}, {"gcc", ""}, {"ld", ""}, {"bison", ""}, {"flex", ""}, {"bc", ""},
	} {
		if err := need(t.tool, t.hint); err != nil {
			return err
		}
	}
	if role != "guest" && role != "host" {
		return fmt.Errorf("unknown role %q: guest or host", role)
	}
	b := filepath.Join(c.Out, "build-"+role)
	if err := os.MkdirAll(b, 0o755); err != nil {
		return err
	}
	_, statErr := os.Stat(filepath.Join(c.KSRC, "arch", "x86", "kvm", "pvm"))
	pvmTree := statErr == nil

	frags := []string{filepath.Join(c.Testbed, "configs", "common.fragment")}
	roleFrag := filepath.Join(c.Testbed, "configs", role+".fragment")
	if !pvmTree {
		raw, err := os.ReadFile(roleFrag)
		if err != nil {
			return err
		}
		var keep []string
		for _, l := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(l, "CONFIG_PVM_GUEST=") || strings.HasPrefix(l, "CONFIG_X86_PIE=") ||
				strings.HasPrefix(l, "CONFIG_KVM_PVM=") {
				continue
			}
			keep = append(keep, l)
		}
		roleFrag = filepath.Join(b, ".role.fragment")
		if err := os.WriteFile(roleFrag, []byte(strings.Join(keep, "\n")), 0o644); err != nil {
			return err
		}
	}
	frags = append(frags, roleFrag)
	var ex bytes.Buffer
	for _, opt := range extra {
		if strings.HasSuffix(opt, "=n") {
			fmt.Fprintf(&ex, "# %s is not set\n", strings.TrimSuffix(opt, "=n"))
		} else {
			fmt.Fprintln(&ex, opt)
		}
	}
	exFrag := filepath.Join(b, ".extra.fragment")
	if err := os.WriteFile(exFrag, ex.Bytes(), 0o644); err != nil {
		return err
	}
	frags = append(frags, exFrag)

	note := ""
	if !pvmTree {
		note = ", no PVM in this tree"
	}
	logf("configuring %s kernel in %s (KSRC=%s)%s", role, b, c.KSRC, note)
	mk := func(args ...string) error {
		return runCmd("", nil, "make", append([]string{"-C", c.KSRC, "O=" + b}, args...)...)
	}
	if err := mk("-s", "defconfig"); err != nil {
		return err
	}
	// merge_config.sh is the kernel's own; it lives in the tree under test.
	if err := runCmd("", nil, filepath.Join(c.KSRC, "scripts", "kconfig", "merge_config.sh"),
		append([]string{"-m", "-O", b, filepath.Join(b, ".config")}, frags...)...); err != nil {
		return err
	}
	if err := mk("-s", "olddefconfig"); err != nil {
		return err
	}

	// merge_config.sh warns but does not fail when the kernel drops an
	// option it was asked for.  For these that is the difference between
	// testing what we think we are testing and testing nothing, so check.
	want := []string{"CONFIG_SERIAL_8250_CONSOLE=y"}
	switch {
	case role == "guest" && pvmTree:
		want = append(want, "CONFIG_PVM_GUEST=y", "CONFIG_X86_PIE=y", "CONFIG_PVH=y")
	case role == "guest":
		want = append(want, "CONFIG_PVH=y")
	case pvmTree:
		want = append(want, "CONFIG_KVM_PVM=m", "CONFIG_KVM_INTEL=m")
	default:
		want = append(want, "CONFIG_KVM_INTEL=m")
	}
	want = append(want, extra...)
	cfg, err := os.ReadFile(filepath.Join(b, ".config"))
	if err != nil {
		return err
	}
	lines := map[string]bool{}
	for _, l := range strings.Split(string(cfg), "\n") {
		lines[l] = true
	}
	for _, opt := range want {
		line := opt
		if strings.HasSuffix(opt, "=n") {
			line = "# " + strings.TrimSuffix(opt, "=n") + " is not set"
		}
		if !lines[line] {
			return fmt.Errorf("%s: %s did not survive olddefconfig", role, opt)
		}
	}

	logf("building %s kernel with -j%d", role, c.Jobs)
	if err := mk(fmt.Sprintf("-j%d", c.Jobs)); err != nil {
		return err
	}
	images := filepath.Join(c.Out, "images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(b, "arch", "x86", "boot", "bzImage"), filepath.Join(images, role+"-bzImage"), 0o644); err != nil {
		return err
	}
	// Two copies of the ELF image.  The full one carries the debug info and
	// is what to point a debugger at; the one qemu boots has it stripped,
	// because with CONFIG_DEBUG_INFO the guest vmlinux is around 450MB and
	// every boot would read all of it.  --strip-debug leaves the Xen ELF
	// notes alone, and XEN_ELFNOTE_PHYS32_ENTRY is how qemu -kernel finds
	// pvh_start_xen(), so that is checked rather than assumed.
	if err := copyFile(filepath.Join(b, "vmlinux"), filepath.Join(images, role+"-vmlinux.debug"), 0o755); err != nil {
		return err
	}
	if err := runCmd("", nil, "objcopy", "--strip-debug", filepath.Join(b, "vmlinux"), filepath.Join(images, role+"-vmlinux")); err != nil {
		return err
	}
	if role == "guest" {
		out, _ := exec.Command("readelf", "-n", filepath.Join(images, "guest-vmlinux")).Output()
		if !bytes.Contains(out, []byte("0x00000012")) {
			return fmt.Errorf("the stripped guest image lost XEN_ELFNOTE_PHYS32_ENTRY")
		}
	}
	if role == "host" {
		// L1 needs modules; they travel on the payload share.
		mods := filepath.Join(c.Out, "modules-host")
		os.RemoveAll(mods)
		if err := mk(fmt.Sprintf("-j%d", c.Jobs), "-s", "modules_install",
			"INSTALL_MOD_PATH="+mods, "INSTALL_MOD_STRIP=1"); err != nil {
			return err
		}
	}
	logf("%s kernel ready: %s", role, filepath.Join(images, role+"-bzImage"))
	return nil
}

// BuildInitrd builds the guest initrd: one static Go binary as /init, in a
// cpio archive.  Nothing else is in the image -- no shell, no libc, no
// busybox -- so a boot that reaches "PVMINIT" has genuinely reached user
// space rather than something a rescue shell papered over.
func BuildInitrd(c Config) error {
	stage, err := os.MkdirTemp("", "pvm-initrd.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	logf("building the Go init (static, no cgo)")
	init := filepath.Join(stage, "init")
	if err := goBuildStatic(filepath.Join(c.Testbed, "initrd"), "./cmd/pvminit", init); err != nil {
		return err
	}
	images := filepath.Join(c.Out, "images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		return err
	}
	raw, err := os.ReadFile(init)
	if err != nil {
		return err
	}
	var cpio bytes.Buffer
	w := newCpio(&cpio)
	for _, d := range []string{"dev", "proc", "run", "sys", "tmp"} {
		w.dir(d)
	}
	// The second name is for a human poking around with rdinit=: a hard
	// link, so the image carries the binary once.
	w.hardlinks([]string{"init", "pvminit"}, raw, 0o755)
	w.close()
	f, err := os.Create(filepath.Join(images, "initrd.cpio.gz"))
	if err != nil {
		return err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	if _, err := io.Copy(gz, &cpio); err != nil {
		return err
	}
	gz.Close()
	f.Close()
	st, _ := os.Stat(filepath.Join(images, "initrd.cpio.gz"))
	logf("initrd ready: %s (%dK)", filepath.Join(images, "initrd.cpio.gz"), st.Size()/1024)
	return nil
}

// BuildHosttests builds the VMM-side test programs (tools/hosttests) into
// c.Out/hosttests, static.
func BuildHosttests(c Config) error {
	dir := filepath.Join(c.Testbed, "tools", "hosttests")
	out := filepath.Join(c.Out, "hosttests")
	os.RemoveAll(out)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	cmds, _ := filepath.Glob(filepath.Join(dir, "cmd", "*"))
	if len(cmds) == 0 {
		return fmt.Errorf("no programs under %s/cmd", dir)
	}
	for _, cmd := range cmds {
		name := filepath.Base(cmd)
		logf("building %s", name)
		if err := goBuildStatic(dir, "./cmd/"+name, filepath.Join(out, name)); err != nil {
			return err
		}
	}
	logf("host tests ready in %s", out)
	return nil
}

// BuildAgent builds the L1 agent (tools/l1agent) into c.Out/l1agent, static.
func BuildAgent(c Config) error {
	out := filepath.Join(c.Out, "l1agent")
	if err := goBuildStatic(filepath.Join(c.Testbed, "tools", "l1agent"), ".", out); err != nil {
		return err
	}
	logf("L1 agent ready: %s", out)
	return nil
}

// BuildSelftests builds the subset of the kernel's own KVM selftests listed
// in configs/kvm-selftests.txt and stages it for the L1 payload.
//
// Two things about this build are not obvious:
//
//   - It needs the tree's own uapi headers.  The selftests reach for
//     KHDR_INCLUDES, which on a distro points at /usr/include, and that
//     asm/kvm.h is older than the tests: the build fails on
//     KVM_X86_QUIRK_NESTED_SVM_SHARED_PAT before it gets anywhere.
//   - It does not honour O= for the final link, so the binaries land in the
//     source tree.  They are gitignored there, so this leaves them alone
//     rather than fighting the kernel's own Makefile.
func BuildSelftests(c Config) error {
	if err := c.NeedKSRC(); err != nil {
		return err
	}
	tests, err := readList(filepath.Join(c.Testbed, "configs", "kvm-selftests.txt"))
	if err != nil {
		return err
	}
	if len(tests) == 0 {
		return fmt.Errorf("no tests listed in configs/kvm-selftests.txt")
	}
	b := filepath.Join(c.Out, "build-host")
	hdr := filepath.Join(b, "usr", "include")
	if _, err := os.Stat(filepath.Join(hdr, "linux", "kvm.h")); err != nil {
		logf("installing uapi headers from %s", c.KSRC)
		if err := runCmd("", nil, "make", "-C", c.KSRC, "O="+b, "headers_install", "INSTALL_HDR_PATH="+filepath.Join(b, "usr"), "-s"); err != nil {
			return err
		}
	}
	src := filepath.Join(c.KSRC, "tools", "testing", "selftests", "kvm")
	logf("building %d selftests (this takes a few minutes the first time)", len(tests))
	// Absolute targets.  The KVM selftests Makefile's link rules are written
	// for $(OUTPUT)/<test>, OUTPUT being the absolute directory; a relative
	// "x86/foo" misses them and falls through to make's builtin %: %.o,
	// which links without libkvm.
	args := []string{"-C", src, "ARCH=x86", "KHDR_INCLUDES=-isystem " + hdr, fmt.Sprintf("-j%d", c.Jobs)}
	for _, t := range tests {
		args = append(args, filepath.Join(src, t))
	}
	logPath := filepath.Join(c.Out, "kvm-selftests-build.log")
	lf, err := os.Create(logPath)
	if err != nil {
		return err
	}
	cmd := exec.Command("make", args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	err = cmd.Run()
	lf.Close()
	if err != nil {
		return fmt.Errorf("selftest build failed -- see %s", logPath)
	}
	stage := filepath.Join(c.Out, "kvm-selftests")
	os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	for _, t := range tests {
		// Flattened, with the directory folded into the name, so the
		// agent can just iterate over one directory.
		if err := copyFile(filepath.Join(src, t), filepath.Join(stage, strings.ReplaceAll(t, "/", "__")), 0o755); err != nil {
			return fmt.Errorf("%s did not build: %w", t, err)
		}
	}
	logf("staged %d selftests", len(tests))
	return nil
}

// BuildRootfs builds the L1 root filesystem: a minimal system with qemu in
// it, so that L1 can be the PVM host running the guest under test.
//
// mmdebstrap can do this without root, but only where unprivileged user
// namespaces are allowed.  Ubuntu 24.04 and later restrict them by default
// (kernel.apparmor_restrict_unprivileged_userns=1), so this picks a mode
// rather than assuming one, says which it picked, and when it does run as
// root it hands the results back to the invoking user -- otherwise every
// later unprivileged step trips over root-owned files.
//
// The image carries the L1 agent itself (static Go, tools/l1agent) as a stub
// at /opt/pvm/agent: it mounts the payload share and execs the agent that
// arrives on it, so iterating on the agent needs no image rebuild.
func BuildRootfs(c Config) (err error) {
	defer func() {
		// Hand out/ back on the way out, however we leave.
		if uid, gid := os.Getenv("SUDO_UID"), os.Getenv("SUDO_GID"); uid != "" && gid != "" {
			exec.Command("chown", "-R", uid+":"+gid, c.Out).Run()
		}
	}()
	for _, t := range []struct{ tool, hint string }{
		{"mmdebstrap", "Debian/Ubuntu: apt install mmdebstrap"},
		{"mkfs.ext4", "Debian/Ubuntu: apt install e2fsprogs"},
	} {
		if err := need(t.tool, t.hint); err != nil {
			return err
		}
	}
	root := filepath.Join(c.Out, "l1-root")
	img := filepath.Join(c.Out, "images", "l1-rootfs.ext4")
	size := envOr("L1_ROOTFS_SIZE", "4G")
	if _, err := os.Stat(filepath.Join(c.Out, "modules-host")); err != nil {
		return fmt.Errorf("no host modules -- build the host kernel first")
	}

	// Which distribution: whatever this machine can actually verify.
	var suite, mirror, components string
	switch {
	case os.Getenv("ROOTFS_SUITE") != "":
		suite = os.Getenv("ROOTFS_SUITE")
		mirror = os.Getenv("ROOTFS_MIRROR")
		if mirror == "" {
			return fmt.Errorf("set ROOTFS_MIRROR alongside ROOTFS_SUITE")
		}
		components = envOr("ROOTFS_COMPONENTS", "main")
	case fileExists("/usr/share/keyrings/ubuntu-archive-keyring.gpg"):
		suite = osReleaseCodename()
		mirror, components = "http://archive.ubuntu.com/ubuntu", "main,universe"
	case fileExists("/usr/share/keyrings/debian-archive-keyring.gpg"):
		suite, mirror, components = "trixie", "http://deb.debian.org/debian", "main"
	default:
		return fmt.Errorf("no usable archive keyring: apt install ubuntu-keyring or debian-archive-keyring")
	}

	// Which mode, in order of how little privilege it needs.
	mode := ""
	switch {
	case exec.Command("unshare", "-Ur", "true").Run() == nil:
		mode = "unshare"
	case need("fakechroot", "") == nil:
		mode = "fakechroot"
	case os.Geteuid() == 0:
		mode = "root"
	default:
		return fmt.Errorf("unprivileged user namespaces are restricted on this machine, so mmdebstrap cannot run unprivileged.  " +
			"Either apt install fakechroot and build as yourself, or run this one step with sudo -- the results are chowned back to you")
	}
	logf("bootstrapping %s from %s, mode=%s", suite, mirror, mode)

	agent := filepath.Join(c.Out, "l1agent")
	if err := BuildAgent(c); err != nil {
		return err
	}

	// What the image carries beyond the distribution, staged here and synced
	// in by mmdebstrap, which owns it as root inside its namespace.  Going
	// through a tarball rather than a directory is what keeps ownership: a
	// directory extracted in an unshare namespace belongs to the invoking
	// user once seen from outside, and an image made from it boots with a
	// setuid mount that is not root's.
	stage, err := os.MkdirTemp("", "pvm-rootfs-stage.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	// L1 exists to run one command.  Wire it as a unit rather than an
	// interactive login: the harness reads the result off the serial line
	// with no one present.
	unit := `[Unit]
Description=PVM testbed agent
After=multi-user.target
[Service]
Type=oneshot
ExecStart=/opt/pvm/agent stub
StandardOutput=journal+console
StandardError=journal+console
[Install]
WantedBy=multi-user.target
`
	files := map[string]string{
		"etc/systemd/system/pvm-agent.service": unit,
		"etc/hostname":                         "pvm-l1\n",
		"etc/fstab":                            "/dev/vda / ext4 defaults 0 1\n",
		// The image carries the modules of the host kernel it was built
		// with, so udev would load kvm_intel at boot and leave kvm-pvm
		// "busy" -- and a suite would then pass under kvm-intel.  The
		// agent loads the vendor it was asked for, from the payload.
		"etc/modprobe.d/pvm-testbed.conf": "blacklist kvm\nblacklist kvm_intel\nblacklist kvm_amd\nblacklist kvm_pvm\n",
	}
	for p, body := range files {
		full := filepath.Join(stage, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return err
		}
	}
	wants := filepath.Join(stage, "etc", "systemd", "system", "multi-user.target.wants")
	os.MkdirAll(wants, 0o755)
	if err := os.Symlink("../pvm-agent.service", filepath.Join(wants, "pvm-agent.service")); err != nil {
		return err
	}
	os.MkdirAll(filepath.Join(stage, "opt", "pvm"), 0o755)
	if err := copyFile(agent, filepath.Join(stage, "opt", "pvm", "agent"), 0o755); err != nil {
		return err
	}
	os.MkdirAll(filepath.Join(stage, "lib"), 0o755)
	if err := runCmd("", nil, "cp", "-a", filepath.Join(c.Out, "modules-host", "lib", "modules"), filepath.Join(stage, "lib")+"/"); err != nil {
		return err
	}

	tarball := filepath.Join(c.Out, "l1-root.tar")
	os.Remove(tarball)
	os.RemoveAll(root)
	// Kept deliberately short: L1 runs one agent and one qemu.
	packages := "systemd-sysv,qemu-system-x86,qemu-utils,kmod,iproute2,procps,less,strace"
	if err := runCmd("", nil, "mmdebstrap", "--mode="+mode, "--variant=minbase",
		"--components="+components, "--include="+packages,
		"--customize-hook=sync-in "+stage+" /",
		// Everything synced in is root's; L1 is disposable and reachable only
		// over its own serial console, so root needs no password.
		`--customize-hook=chroot "$1" chown -R root:root /opt/pvm /lib/modules /etc/systemd/system/pvm-agent.service /etc/systemd/system/multi-user.target.wants /etc/hostname /etc/fstab /etc/modprobe.d/pvm-testbed.conf`,
		`--customize-hook=chroot "$1" passwd -d root`,
		suite, tarball, mirror); err != nil {
		return err
	}

	logf("making %s (%s)", img, size)
	os.MkdirAll(filepath.Dir(img), 0o755)
	os.Remove(img)
	if err := runCmd("", nil, "mkfs.ext4", "-q", "-L", "pvm-l1", "-d", tarball, img, size); err != nil {
		return err
	}
	os.Remove(tarball)
	logf("L1 rootfs ready: %s", img)
	return nil
}

// BuildRef builds a complete host and guest image set from one revision of a
// git tree into c.Out/refs/<name>/, for comparing kernels side by side --
// upstream with kvm-intel against the PVM series with kvm-pvm.  The source is
// a detached worktree at out/refs/<name>/src: no branch is created and the
// tree's own checkout is not touched.  The rootfs, initrd, agent, host tests
// and KVM selftests are the testbed's, linked in.  Nothing is rebuilt when
// the revision and the variant are the ones already built.
func BuildRef(c Config, name, gitDir, rev, variant string) error {
	if variant != "timing" && variant != "stats" {
		return fmt.Errorf("variant: timing or stats")
	}
	refOut := filepath.Join(c.Out, "refs", name)
	src := filepath.Join(refOut, "src")
	shaOut, err := exec.Command("git", "-C", gitDir, "rev-parse", "--verify", rev+"^{commit}").Output()
	if err != nil {
		return fmt.Errorf("%s: no revision %s", gitDir, rev)
	}
	sha := strings.TrimSpace(string(shaOut))
	stamp := sha + " " + variant
	os.MkdirAll(filepath.Join(refOut, "images"), 0o755)
	built, _ := os.ReadFile(filepath.Join(refOut, "built"))
	if strings.TrimSpace(string(built)) == stamp && fileExists(filepath.Join(refOut, "images", "host-bzImage")) &&
		fileExists(filepath.Join(refOut, "images", "guest-vmlinux")) {
		logf("%s: %s (%.12s) %s already built", name, rev, sha, variant)
	} else {
		if fileExists(src) {
			if err := runCmd("", nil, "git", "-C", src, "checkout", "-q", "--detach", sha); err != nil {
				return err
			}
		} else if err := runCmd("", nil, "git", "-C", gitDir, "worktree", "add", "-q", "--detach", src, sha); err != nil {
			return err
		}
		var extra []string
		if variant == "stats" && fileExists(filepath.Join(src, "arch", "x86", "kvm", "pvm")) {
			extra = []string{"CONFIG_KVM_PVM_STATS=y"}
		}
		os.Remove(filepath.Join(refOut, "built"))
		rc := c.With(refOut, src)
		if err := BuildKernel(rc, "guest", nil); err != nil {
			return err
		}
		if err := BuildKernel(rc, "host", extra); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(refOut, "built"), []byte(stamp+"\n"), 0o644); err != nil {
			return err
		}
	}
	// What every set shares with the testbed's own.
	for _, f := range []string{"images/l1-rootfs.ext4", "images/initrd.cpio.gz", "hosttests", "kvm-selftests", "kut", "l1agent"} {
		from := filepath.Join(c.Out, f)
		if !fileExists(from) {
			continue
		}
		to := filepath.Join(refOut, f)
		os.Remove(to)
		if err := os.Symlink(from, to); err != nil {
			return err
		}
	}
	logf("%s: images in %s", name, filepath.Join(refOut, "images"))
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func osReleaseCodename() string {
	raw, _ := os.ReadFile("/etc/os-release")
	for _, l := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(l, "VERSION_CODENAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// A newc cpio writer: just enough for the initrd -- directories and regular
// files, owned by root, one link each.
type cpioWriter struct {
	w   io.Writer
	ino int
}

func newCpio(w io.Writer) *cpioWriter { return &cpioWriter{w: w, ino: 1} }

func (c *cpioWriter) header(name string, mode uint32, size int) {
	c.headerLinks(name, mode, size, c.ino, 1)
	c.ino++
}

func (c *cpioWriter) headerLinks(name string, mode uint32, size, ino, nlink int) {
	namesize := len(name) + 1
	fmt.Fprintf(c.w, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		ino, mode, 0, 0, nlink, 0, size, 0, 0, 0, 0, namesize, 0)
	io.WriteString(c.w, name+"\x00")
	c.pad(110 + namesize)
}

func (c *cpioWriter) pad(n int) {
	if r := n % 4; r != 0 {
		c.w.Write(make([]byte, 4-r))
	}
}

func (c *cpioWriter) dir(name string) { c.header(name, 0o040755, 0) }

func (c *cpioWriter) file(name string, data []byte, perm uint32) {
	c.header(name, 0o100000|perm, len(data))
	c.w.Write(data)
	c.pad(len(data))
}

// hardlinks writes names as links of one file: in newc the data goes with
// the last of them, the others have size 0.
func (c *cpioWriter) hardlinks(names []string, data []byte, perm uint32) {
	ino := c.ino
	c.ino++
	for i, n := range names {
		size := 0
		if i == len(names)-1 {
			size = len(data)
		}
		c.headerLinks(n, 0o100000|perm, size, ino, len(names))
		if size > 0 {
			c.w.Write(data)
			c.pad(len(data))
		}
	}
}

func (c *cpioWriter) close() { c.header("TRAILER!!!", 0, 0) }

// sortedKeys is for deterministic output.
func sortedKeys(m map[string]bool) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
