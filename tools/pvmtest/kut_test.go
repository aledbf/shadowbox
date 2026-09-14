package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseUnittestsCfg(t *testing.T) {
	cfg := `
[x2apic]
file = vmexit.flat
smp = 2
test_args = 'toggle_cr4_pge'
groups = vmexit

# comment
[pcid-enabled]
file = pcid.flat
qemu_params = -cpu qemu64,+pcid,+invpcid
arch = x86_64
timeout = 240s
check = /sys/module/kvm_intel/parameters/ept=Y
`
	p := filepath.Join(t.TempDir(), "unittests.cfg")
	os.WriteFile(p, []byte(cfg), 0o644)
	tests, err := parseUnittestsCfg(p)
	if err != nil || len(tests) != 2 {
		t.Fatalf("got %v %v", tests, err)
	}
	got := kutManifest(tests)
	want := "x2apic\tvmexit.flat\t2\t90\t\t-append 'toggle_cr4_pge'\n" +
		"pcid-enabled\tpcid.flat\t1\t240\t/sys/module/kvm_intel/parameters/ept=Y\t-cpu qemu64,+pcid,+invpcid\n"
	if got != want {
		t.Fatalf("manifest:\n%q\nwant\n%q", got, want)
	}
	if _, err := selectKUT(tests, []string{"nope"}); err == nil || !strings.Contains(err.Error(), "not in") {
		t.Fatalf("unknown name: %v", err)
	}
}

func TestKUTConfig(t *testing.T) {
	if c := kutConfig(Item{Keys: map[string]string{"mod": "ept=0"}}); c != "shadow" {
		t.Fatal(c)
	}
	if c := kutConfig(Item{Keys: map[string]string{}}); c != "ept" {
		t.Fatal(c)
	}
}
