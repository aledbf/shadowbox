package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// kvm-unit-tests run in L1 under kvm-intel, not under kvm-pvm.  Their guests
// are bare x86 programs at CPL0 that take exceptions on purpose, which a PVM
// VM cannot deliver without a PVM-aware guest.  What they are for is the code
// the PVM series shares with every other KVM user: the shadow MMU
// (kvm_intel ept=0), the emulator and vcpu_enter_guest().  A battery runs
// them against kernels with and without the series and holds both to the
// same configs/kut-expect.txt.
//
// The tests come from outside the testbed, like the kernel: KUT_GIT names a
// kvm-unit-tests tree (default: a clone under CACHE) and KUT_REV a revision.

const kutUpstream = "https://gitlab.com/kvm-unit-tests/kvm-unit-tests.git"

// kutTest is one [section] of x86/unittests.cfg.
type kutTest struct {
	Name, File, Smp, Timeout, Check, Accel, Arch, Groups, Params, Args string
}

func parseUnittestsCfg(path string) ([]kutTest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []kutTest
	var cur *kutTest
	section := regexp.MustCompile(`^\[(.+)\]$`)
	kv := regexp.MustCompile(`^([a-z_]+)\s*=\s*(.*)$`)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if m := section.FindStringSubmatch(l); m != nil {
			out = append(out, kutTest{Name: m[1], Smp: "1", Timeout: "90"})
			cur = &out[len(out)-1]
			continue
		}
		m := kv.FindStringSubmatch(l)
		if m == nil || cur == nil {
			continue
		}
		switch v := m[2]; m[1] {
		case "file":
			cur.File = v
		case "smp":
			cur.Smp = v
		case "timeout":
			cur.Timeout = strings.TrimSuffix(v, "s")
		case "check":
			cur.Check = v
		case "accel":
			cur.Accel = v
		case "arch":
			cur.Arch = v
		case "groups":
			cur.Groups = v
		case "qemu_params", "extra_params":
			cur.Params = strings.TrimSpace(cur.Params + " " + v)
		case "test_args":
			cur.Args = v
		}
	}
	return out, sc.Err()
}

// kutManifest is what the agent reads: one test per line, tab separated --
// name, flat file, smp, timeout seconds, check, qemu arguments.  test_args
// become "-append <args>", quoted as unittests.cfg quoted them.
func kutManifest(tests []kutTest) string {
	var b strings.Builder
	for _, t := range tests {
		args := t.Params
		if t.Args != "" {
			args = strings.TrimSpace(args + " -append " + t.Args)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\n", t.Name, t.File, t.Smp, t.Timeout, t.Check, args)
	}
	return b.String()
}

// selectKUT picks the listed tests out of unittests.cfg, in the list's order.
func selectKUT(all []kutTest, names []string) ([]kutTest, error) {
	byName := map[string]kutTest{}
	for _, t := range all {
		byName[t.Name] = t
	}
	var out []kutTest
	for _, n := range names {
		t, ok := byName[n]
		switch {
		case !ok:
			return nil, fmt.Errorf("%s: not in x86/unittests.cfg", n)
		case t.Arch == "i386":
			return nil, fmt.Errorf("%s: i386 only", n)
		case t.Accel == "tcg":
			return nil, fmt.Errorf("%s: tcg only", n)
		case strings.Contains(t.Params+t.Args, "`"):
			return nil, fmt.Errorf("%s: needs shell expansion", n)
		}
		out = append(out, t)
	}
	return out, nil
}

// BuildKUT builds kvm-unit-tests at KUT_REV and stages the tests listed in
// configs/kut-tests.txt into c.Out/kut.
func BuildKUT(c Config) error {
	gitDir := envOr("KUT_GIT", filepath.Join(c.Cache, "kvm-unit-tests"))
	rev := envOr("KUT_REV", "master")
	if !fileExists(gitDir) {
		logf("cloning %s into %s", kutUpstream, gitDir)
		if err := runCmd("", nil, "git", "clone", "-q", kutUpstream, gitDir); err != nil {
			return err
		}
	}
	shaOut, err := exec.Command("git", "-C", gitDir, "rev-parse", "--verify", rev+"^{commit}").Output()
	if err != nil {
		return fmt.Errorf("%s: no revision %s", gitDir, rev)
	}
	sha := strings.TrimSpace(string(shaOut))
	root := filepath.Join(c.Out, "kut-build")
	src := filepath.Join(root, "src")
	if fileExists(src) {
		if err := runCmd("", nil, "git", "-C", src, "checkout", "-q", "--detach", sha); err != nil {
			return err
		}
	} else {
		os.MkdirAll(root, 0o755)
		if err := runCmd("", nil, "git", "-C", gitDir, "worktree", "add", "-q", "--detach", src, sha); err != nil {
			return err
		}
	}
	names, err := readList(filepath.Join(c.Testbed, "configs", "kut-tests.txt"))
	if err != nil {
		return err
	}
	all, err := parseUnittestsCfg(filepath.Join(src, "x86", "unittests.cfg"))
	if err != nil {
		return err
	}
	tests, err := selectKUT(all, names)
	if err != nil {
		return fmt.Errorf("configs/kut-tests.txt: %w", err)
	}
	logf("building kvm-unit-tests %.12s", sha)
	logPath := filepath.Join(root, "build.log")
	lf, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer lf.Close()
	for _, argv := range [][]string{{"./configure", "--arch=x86_64"}, {"make", fmt.Sprintf("-j%d", c.Jobs)}} {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = src, lf, lf
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("kvm-unit-tests %s failed -- see %s", argv[0], logPath)
		}
	}
	stage := filepath.Join(c.Out, "kut")
	os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	files := map[string]bool{}
	for _, t := range tests {
		files[t.File] = true
	}
	for _, f := range sortedKeys(files) {
		if err := copyFile(filepath.Join(src, "x86", f), filepath.Join(stage, f), 0o644); err != nil {
			return fmt.Errorf("%s did not build: %w", f, err)
		}
	}
	manifest := fmt.Sprintf("# kvm-unit-tests %s\n", sha) + kutManifest(tests)
	if err := writeFile(filepath.Join(stage, "tests.txt"), manifest); err != nil {
		return err
	}
	logf("staged %d kvm-unit-tests (%d binaries) in %s", len(tests), len(files), stage)
	return nil
}

// kutConfig names the kvm_intel configuration an item runs the tests in,
// which is the column configs/kut-expect.txt is keyed on.
func kutConfig(it Item) string {
	for _, m := range it.List("mod") {
		if m == "ept=0" || m == "ept=N" {
			return "shadow"
		}
	}
	return "ept"
}

// CheckKUT compares the KUT: lines of a log against configs/kut-expect.txt
// for one configuration, in both directions, as CheckSelftests does.
func CheckKUT(c Config, logPath, config string) error {
	return checkExpected(filepath.Join(c.Testbed, "configs", "kut-expect.txt"), logPath, "KUT: ", config)
}

// kutSummary counts verdicts in a log, for the battery's summary line.
func kutSummary(logText string) string {
	n := map[string]int{}
	for _, l := range strings.Split(logText, "\n") {
		if f := strings.Fields(l); len(f) >= 3 && f[0] == "KUT:" {
			n[f[2]]++
		}
	}
	var keys []string
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Itoa(n[k]))
	}
	return strings.Join(parts, " ")
}
