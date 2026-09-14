package main

import (
	"strings"
	"testing"
)

// The qemu command line the bash run-l1.sh used to build, for the same
// inputs: the rootfs as a snapshot, the payload over 9p read-only, and the
// agent's parameters on L1's kernel command line in the same order.
func TestL1Argv(t *testing.T) {
	c := Config{Qemu: "qemu-system-x86_64", Out: "/o", L1CPUs: "8", L1Mem: "8G"}
	o := L1Opts{Suite: "perf", Vendor: "pvm", Guest: []string{"pvmtest.only=perf/page-fault", "pvm_direct_pf=off"},
		Mod: []string{"a=1"}, CPUs: "8", Profile: "perf/syscall", L1Args: []string{"pti=on"}, Accel: "kvm", CPU: "host"}
	got := strings.Join(L1Argv(c, o, "/p"), " ")
	want := "qemu-system-x86_64 -machine q35,accel=kvm -cpu host -smp 8 -m 8G -kernel /o/images/host-bzImage " +
		"-drive file=/o/images/l1-rootfs.ext4,if=virtio,format=raw,snapshot=on " +
		"-append root=/dev/vda rw console=ttyS0,115200 panic=-1 pvmtest.suite=perf pvmtest.vendor=pvm " +
		"pvmtest.profile_case=perf/syscall pvmtest.guest_append=pvmtest.only=perf/page-fault,pvm_direct_pf=off " +
		"pvmtest.mod_args=a=1 pvmtest.guest_cpus=8 pti=on systemd.mask=serial-getty@ttyS0.service systemd.show_status=false " +
		"-virtfs local,path=/p,mount_tag=payload,security_model=none,readonly=on -nographic -no-reboot -display none -serial mon:stdio"
	if got != want {
		t.Errorf("argv:\n got %s\nwant %s", got, want)
	}
}

func TestL1OptsFromItem(t *testing.T) {
	e := &Env{}
	it := Item{Kind: "run", Name: "x", Keys: map[string]string{
		"suite": "security", "cases": "a,b", "stats": "on", "guest": "g=1", "l1": "tcg-la57", "pti": "on", "mod": "m=2",
	}}
	o, err := e.l1Opts(Boot{Item: it, Vendor: "pvm", CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(o.Guest, ",") != "pvmtest.only=a|b,pvmtest.statsmsr=0x4b564d2f,g=1" {
		t.Errorf("guest: %v", o.Guest)
	}
	if o.Accel != "tcg" || o.CPU != "max,la57=on" || strings.Join(o.L1Args, " ") != "pti=on pvmtest.guest_timeout=15000" {
		t.Errorf("l1: %+v", o)
	}
	if o.CPUs != "1" || strings.Join(o.Mod, ",") != "m=2" {
		t.Errorf("cpus/mod: %+v", o)
	}
}
