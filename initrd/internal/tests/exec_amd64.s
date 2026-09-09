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
