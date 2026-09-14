#include "textflag.h"

// func cpuidCount(leaf, sub uint32) (eax, ebx, ecx, edx uint32)
TEXT ·cpuidCount(SB), NOSPLIT, $0-24
	MOVL leaf+0(FP), AX
	MOVL sub+4(FP), CX
	CPUID
	MOVL AX, eax+8(FP)
	MOVL BX, ebx+12(FP)
	MOVL CX, ecx+16(FP)
	MOVL DX, edx+20(FP)
	RET

// func rdpkru() uint32
TEXT ·rdpkru(SB), NOSPLIT, $0-4
	XORL CX, CX
	// rdpkru
	BYTE $0x0f; BYTE $0x01; BYTE $0xee
	MOVL AX, ret+0(FP)
	RET
