package main

import (
	"strings"
	"testing"
)

const sampleGuest = `L1: loading pvm from /mnt/payload/kvm-pvm.ko
G[q35]: PVMTEST-MARK: 2 perf/page-fault/before #END
[    1.0] kvm_pvm: PVMSTATS: mark=2 vcpu=0 exits=10 pf_taken=4
[    1.0] kvm_pvm: PVMSTATS: mark=2 vcpu=1 exits=1 pf_taken=0
[    1.0] kvm_pvm: PVMSTATS-VM: mark=2 pages_4k=5
L1: dmesg: [    1.0] kvm_pvm: PVMSTATS: mark=2 vcpu=0 exits=999
G[q35]: ok 1 - perf/page-fault (311ms)
G[q35]: PVMTEST-METRIC: perf/page-fault.ns_per_fault 4658.1 ns #END
G[q35]: PVMTEST-MARK: 3 perf/page-fault/after #END
[    1.1] kvm_pvm: PVMSTATS: mark=3 vcpu=0 exits=110 pf_taken=54
[    1.1] kvm_pvm: PVMSTATS: mark=3 vcpu=1 exits=2 pf_taken=0
[    1.1] kvm_pvm: PVMSTATS-VM: mark=3 pages_4k=9
G[q35]: not ok 2 - time/monotonic (30.0s)
G[q35]: not ok 3 - security/priv-in[    2.3] e1000e: EEE TX LPI TIMER
G[q35]: # failed: time/monotonic security/priv-insn/invd
G[q35]: PVMTEST-RESULT: fail tag=pvm-guest suite=default pass=1 fail=2
H: PVMHOSTTEST-RESULT: ok pass=21 fail=0
H: PVMHOSTTEST-RESULT: fail pass=12 fail=1
SELFTEST: dirty_log_test pass rc=0
[    3.0] WARNING: CPU: 1 PID: 7 at arch/x86/kvm/mmu/mmu.c:1 foo
`

func TestParseLog(t *testing.T) {
	o := ParseLog(strings.NewReader(sampleGuest))
	if len(o.Passed) != 1 || o.Passed[0] != "perf/page-fault" {
		t.Errorf("passed: %v", o.Passed)
	}
	want := []string{"time/monotonic", "security/priv-in", "security/priv-insn/invd"}
	if strings.Join(o.Failed, " ") != strings.Join(want, " ") {
		t.Errorf("failed: %v", o.Failed)
	}
	if got := o.Unexpected([]string{"time/monotonic", "security/priv-insn/invd"}); len(got) != 0 {
		t.Errorf("unexpected with the garbled name allowed: %v", got)
	}
	if got := o.Unexpected([]string{"time/monotonic"}); len(got) != 2 {
		t.Errorf("unexpected: %v", got)
	}
	if o.HostPass != 33 || o.HostFail != 1 {
		t.Errorf("host tests: %d/%d", o.HostPass, o.HostFail)
	}
	if o.Selftests != 1 {
		t.Errorf("selftests: %d", o.Selftests)
	}
	if len(o.Metrics) != 1 || o.Metrics[0].Value != 4658.1 {
		t.Errorf("metrics: %v", o.Metrics)
	}
	if len(o.Complaints) != 1 {
		t.Errorf("complaints: %v", o.Complaints)
	}
	pairs, keys := o.StatsPairs()
	if len(pairs) != 1 || pairs[0].Name != "perf/page-fault" {
		t.Fatalf("pairs: %v", pairs)
	}
	if pairs[0].Deltas["exits"] != 101 || pairs[0].Deltas["pf_taken"] != 50 ||
		pairs[0].Deltas["vm.pages_4k"] != 4 {
		t.Errorf("deltas: %v (keys %v)", pairs[0].Deltas, keys)
	}
}

func TestParseBattery(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/b"
	text := `# a battery
set host=stats l1=kvm
run default suite=default
run la57 suite=default l1=tcg-la57 allow-fail=time/monotonic   # TCG
ab dpf suite=perf cases=perf/fault-rounds a.guest=pvm_direct_pf=off b.guest=pvm_direct_pf=on cpus=1,8
`
	if err := writeFile(path, text); err != nil {
		t.Fatal(err)
	}
	b, err := ParseBattery(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Items) != 3 {
		t.Fatalf("items: %v", b.Items)
	}
	if b.Items[1].Get("l1", "") != "tcg-la57" || b.Items[0].Get("host", "") != "stats" {
		t.Errorf("keys: %v", b.Items)
	}
	a := b.Items[2].Variant("a")
	if a.Get("guest", "") != "pvm_direct_pf=off" || a.Get("cases", "") != "perf/fault-rounds" {
		t.Errorf("variant: %v", a)
	}
	if _, err := ParseBattery(writeTemp(t, "run x bogus=1\n")); err == nil {
		t.Errorf("an unknown key was accepted")
	}
}

func writeTemp(t *testing.T, text string) string {
	path := t.TempDir() + "/b"
	if err := writeFile(path, text); err != nil {
		t.Fatal(err)
	}
	return path
}
