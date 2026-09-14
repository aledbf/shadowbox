package harness

import (
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/aledbf/pvm-testbed/tools/hosttests/internal/kvm"
)

func capture(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	f()
	os.Stdout = saved
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// The lines harness.h's ok() and nok() print, and the plan main() prints.
func TestOutputContract(t *testing.T) {
	pass, fail = 0, 0
	got := capture(t, func() {
		Case = "pvm/a"
		Ok("pinned at %s", Hex(uint64(0x102000)))
		Case = "pvm/b"
		Nok("read back %s, wrote %s", Hex(uint64(0)), Hex(uint64(0x102000)))
		Case = "pvm/c"
		Ok("%d%% done", 100)
		if code := Finish(); code != 1 {
			t.Errorf("Finish() = %d, want 1", code)
		}
	})
	want := "ok 1 - pvm/a (pinned at 0x102000)\n" +
		"not ok 2 - pvm/b: read back 0, wrote 0x102000\n" +
		"ok 3 - pvm/c (100% done)\n" +
		"1..3\n" +
		"PVMHOSTTEST-RESULT: fail pass=2 fail=1\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	pass, fail = 0, 0
	got = capture(t, func() {
		Case = "pvm/a"
		Ok("x")
		if code := Finish(); code != 0 {
			t.Errorf("Finish() = %d, want 0", code)
		}
	})
	if want := "ok 1 - pvm/a (x)\n1..1\nPVMHOSTTEST-RESULT: ok pass=1 fail=0\n"; got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	got = capture(t, Skip)
	if want := "1..0 # SKIP kvm_pvm is not the loaded vendor module\n"; got != want {
		t.Errorf("skip line %q, want %q", got, want)
	}
	pass, fail = 0, 0
}

func TestHex(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{Hex(uint64(0)), "0"},
		{Hex(uint32(0x202)), "0x202"},
		{Hex(uint16(0x2b)), "0x2b"},
		{Hex(^uint64(0)), "0xffffffffffffffff"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("Hex = %q, want %q", c.got, c.want)
		}
	}
}

func TestStrerror(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "Success"},
		{syscall.Errno(0), "Success"},
		{syscall.EINVAL, "Invalid argument"},
		{syscall.EINTR, "Interrupted system call"},
		{syscall.EEXIST, "File exists"},
		{syscall.Errno(41), "Unknown error 41"},
		{syscall.Errno(200), "Unknown error 200"},
	}
	for _, c := range cases {
		if got := Strerror(c.err); got != c.want {
			t.Errorf("Strerror(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestExitReasonName(t *testing.T) {
	if ExitReasonName(kvm.EXIT_INTERNAL_ERROR) != "INTERNAL_ERROR" || ExitReasonName(3) != "?" {
		t.Error("exit reason names")
	}
}

func TestCPUIDHas(t *testing.T) {
	c := new(CPUID2)
	c.Nent = 2
	c.Entries[0] = kvm.CPUIDEntry2{Function: 7, Index: 1, Flags: kvm.CPUID_FLAG_SIGNIFCANT_INDEX, ECX: 0xffffffff}
	c.Entries[1] = kvm.CPUIDEntry2{Function: 7, Index: 0, Flags: kvm.CPUID_FLAG_SIGNIFCANT_INDEX, EBX: 1 << 7}
	if !CPUIDHas(c, 7, 0, 1, 7) {
		t.Error("leaf 7.0 ebx bit 7 not found past the 7.1 entry")
	}
	if CPUIDHas(c, 7, 0, 2, 3) {
		t.Error("leaf 7.0 ecx bit 3 taken from the 7.1 entry")
	}
	if CPUIDHas(c, 0xd, 0, 0, 0) {
		t.Error("absent leaf reported")
	}
}
