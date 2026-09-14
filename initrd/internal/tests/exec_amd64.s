#include "textflag.h"

// func callAddr(fn uintptr)
//
// Call an arbitrary address.  Every user of this expects the call to
// fault -- either because the bytes there are a privileged instruction
// that must trap at CPL3, or because the page they live on is not
// executable -- so the only thing that matters on the way in is that
// nothing else faults first.
//
// NOSPLIT and a zero frame: there is no stack growth check to run, and
// the callee is either a lone RET, which clobbers nothing, or an
// instruction that never completes.
TEXT ·callAddr(SB), NOSPLIT, $0-8
	MOVQ	fn+0(FP), AX
	CALL	AX
	RET

// func readAddr(p uintptr) byte
//
// One byte from an arbitrary address, without the compiler being able to
// prove anything about it.  Used to establish that guest kernel memory
// and the host's window are not readable from guest user mode.
TEXT ·readAddr(SB), NOSPLIT, $0-9
	MOVQ	p+0(FP), AX
	MOVBLZX	(AX), BX
	MOVB	BX, ret+8(FP)
	RET

// func cpuidRaw(leaf, sub uint32) (eax, ebx, ecx, edx uint32)
//
// CPUID as the hardware answers it to this privilege level.  Under PVM
// that is deliberately not the same thing as the CPUID the guest kernel
// sees: the kernel's reads go through pv_ops.cpu.cpuid and the synthetic
// "invlpg; cpuid" sequence, which the hypervisor emulates, while a plain
// CPUID at CPL3 is not trapped at all.
TEXT ·cpuidRaw(SB), NOSPLIT, $0-24
	MOVL	leaf+0(FP), AX
	MOVL	sub+4(FP), CX
	CPUID
	MOVL	AX, eax+8(FP)
	MOVL	BX, ebx+12(FP)
	MOVL	CX, ecx+16(FP)
	MOVL	DX, edx+20(FP)
	RET

// func rdpkru() uint32 -- RDPKRU is 0F 01 EE
//
// The PKRU this thread runs on.  RDPKRU needs ECX zero; spelled as bytes
// because the assembler has no mnemonic for it.
TEXT ·rdpkru(SB), NOSPLIT, $0-4
	XORL	CX, CX
	BYTE	$0x0f; BYTE $0x01; BYTE $0xee
	MOVL	AX, ret+0(FP)
	RET
