// Package x86 is the two instructions the host tests execute directly on the
// host: CPUID and RDPKRU.  Go has no inline assembly, so they are two tiny
// assembly functions.
package x86

func cpuidCount(leaf, sub uint32) (eax, ebx, ecx, edx uint32)

func rdpkru() uint32

// CPUIDCount runs CPUID with @leaf in eax and @sub in ecx, and returns
// eax, ebx, ecx, edx in that order.
func CPUIDCount(leaf, sub uint32) [4]uint32 {
	a, b, c, d := cpuidCount(leaf, sub)
	return [4]uint32{a, b, c, d}
}

// RDPKRU returns this thread's PKRU.  It raises #UD when CR4.PKE is clear,
// exactly as the instruction does.
func RDPKRU() uint32 {
	return rdpkru()
}
