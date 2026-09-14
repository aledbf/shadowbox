// Package harness is the shared scaffolding for the host-side PVM tests:
// TAP-ish result lines, a vCPU with guest memory behind it, and the ioctl
// wrappers the cases use.
//
// Not the KVM selftests library: it builds a guest that expects to run at
// CPL0 with its own page tables, which is exactly what a PVM VM does not give
// it.
//
// The output contract is the one the C harness (hosttests/harness.h) had,
// byte for byte: "ok N - <case> (<detail>)", "not ok N - <case>: <message>",
// then "1..N" and "PVMHOSTTEST-RESULT: ok|fail pass=P fail=F".
package harness

import (
	"fmt"
	"os"
)

var (
	pass, fail int

	// Case is the name the next Ok or Nok line is reported under.
	Case string
)

// Ok reports the current case as passed; the detail goes in parentheses.
func Ok(format string, a ...any) {
	pass++
	line := fmt.Sprintf("ok %d - %s (", pass+fail, Case) + fmt.Sprintf(format, a...) + ")\n"
	os.Stdout.WriteString(line)
}

// Nok reports the current case as failed.
func Nok(format string, a ...any) {
	fail++
	line := fmt.Sprintf("not ok %d - %s: ", pass+fail, Case) + fmt.Sprintf(format, a...) + "\n"
	os.Stdout.WriteString(line)
}

// Pass and Fail are the counts so far.
func Pass() int { return pass }
func Fail() int { return fail }

// Die gives up on the whole program: @err is what errno was.
func Die(what string, err error) {
	fmt.Fprintf(os.Stderr, "bail out! %s: %s\n", what, Strerror(err))
	fmt.Fprintf(os.Stdout, "Bail out! %s: %s\n", what, Strerror(err))
	os.Exit(2)
}

// Skip prints the plan of a program that has nothing to run here.
func Skip() {
	os.Stdout.WriteString("1..0 # SKIP kvm_pvm is not the loaded vendor module\n")
}

// Finish prints the plan and the result line, and returns the exit code.
func Finish() int {
	result := "ok"
	if fail != 0 {
		result = "fail"
	}
	fmt.Printf("1..%d\n", pass+fail)
	fmt.Printf("PVMHOSTTEST-RESULT: %s pass=%d fail=%d\n", result, pass, fail)
	if fail != 0 {
		return 1
	}
	return 0
}

// Hex formats @v as C's "%#x" does: 0x-prefixed, except that zero is a bare
// "0".  Go's %#x prints "0x0", which is not what the C tests printed.
func Hex[T ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr | ~int | ~int32 | ~int64](v T) string {
	if v == 0 {
		return "0"
	}
	return fmt.Sprintf("%#x", v)
}

// B2I is C's promotion of a bool to int in a printf argument.
func B2I(b bool) int {
	if b {
		return 1
	}
	return 0
}
