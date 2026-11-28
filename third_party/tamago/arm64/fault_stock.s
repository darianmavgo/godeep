// godeep patch: see exception.go FaultPanics and GODEEP_PATCHES.md.

//go:build !godeepruntime

#include "textflag.h"

// faultPanic: stock TamaGo runtime, no runtime·tamagoFault: every
// exception goes to SystemExceptionHandler.
TEXT ·faultPanic(SB),NOSPLIT|NOFRAME,$0
	RET
