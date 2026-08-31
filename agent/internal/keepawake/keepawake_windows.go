package keepawake

import "syscall"

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procSetThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
)

// EXECUTION_STATE flags for SetThreadExecutionState - see
// https://learn.microsoft.com/windows/win32/api/winbase/nf-winbase-setthreadexecutionstate
const (
	esContinuous       = 0x80000000
	esSystemRequired   = 0x00000001
	esAwaymodeRequired = 0x00000040
)

// Start asks Windows to keep the system awake (ES_SYSTEM_REQUIRED) for as
// long as this state is held. ES_AWAYMODE_REQUIRED additionally lets a
// desktop that has "away mode" enabled stay fully awake and working with
// its display off rather than actually sleeping - harmless, and a no-op,
// on a machine that doesn't have away mode configured.
func Start() (stop func()) {
	_, _, _ = procSetThreadExecutionState.Call(uintptr(esContinuous | esSystemRequired | esAwaymodeRequired))
	return func() {
		_, _, _ = procSetThreadExecutionState.Call(uintptr(esContinuous))
	}
}
