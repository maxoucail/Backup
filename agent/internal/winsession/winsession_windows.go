//go:build windows

// Package winsession lets the Windows Service cross Session 0 isolation:
// a service has no desktop of its own, so to show the setup wizard or a
// progress popup it must launch a small helper process *inside the
// logged-on user's session* using their own access token. This is the
// standard, long-documented pattern for services that need to interact
// with the console user (Microsoft's own CreateProcessAsUser samples use
// exactly these five APIs in this order).
package winsession

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	wtsapi32 = syscall.NewLazyDLL("wtsapi32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	userenv  = syscall.NewLazyDLL("userenv.dll")

	procWTSGetActiveConsoleSessionId = kernel32.NewProc("WTSGetActiveConsoleSessionId")
	procWTSQueryUserToken            = wtsapi32.NewProc("WTSQueryUserToken")
	procDuplicateTokenEx             = advapi32.NewProc("DuplicateTokenEx")
	procCreateEnvironmentBlock       = userenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock      = userenv.NewProc("DestroyEnvironmentBlock")
	procCreateProcessAsUserW         = advapi32.NewProc("CreateProcessAsUserW")
	procGetUserProfileDirectoryW     = userenv.NewProc("GetUserProfileDirectoryW")
)

const (
	tokenImpersonation       = 2 // SecurityImpersonation
	tokenPrimary             = 1 // TokenPrimary
	createUnicodeEnvironment = 0x00000400
	createNoWindow           = 0x08000000
)

type startupInfo struct {
	Cb              uint32
	lpReserved      *uint16
	lpDesktop       *uint16
	lpTitle         *uint16
	dwX, dwY        uint32
	dwXSize         uint32
	dwYSize         uint32
	dwXCountChars   uint32
	dwYCountChars   uint32
	dwFillAttribute uint32
	dwFlags         uint32
	wShowWindow     uint16
	cbReserved2     uint16
	lpReserved2     *byte
	hStdInput       syscall.Handle
	hStdOutput      syscall.Handle
	hStdError       syscall.Handle
}

type processInformation struct {
	hProcess    syscall.Handle
	hThread     syscall.Handle
	dwProcessId uint32
	dwThreadId  uint32
}

func activeConsoleSessionID() (uint32, error) {
	r, _, _ := procWTSGetActiveConsoleSessionId.Call()
	sid := uint32(r)
	if sid == 0xFFFFFFFF {
		return 0, fmt.Errorf("aucune session console active (personne n'est connecté)")
	}
	return sid, nil
}

func consoleUserToken() (syscall.Handle, error) {
	sid, err := activeConsoleSessionID()
	if err != nil {
		return 0, err
	}
	var userToken syscall.Handle
	r, _, err := procWTSQueryUserToken.Call(uintptr(sid), uintptr(unsafe.Pointer(&userToken)))
	if r == 0 {
		return 0, fmt.Errorf("WTSQueryUserToken: %w", err)
	}
	return userToken, nil
}

func primaryToken() (syscall.Handle, error) {
	impToken, err := consoleUserToken()
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(impToken)

	var primary syscall.Handle
	const tokenAllAccess = 0xF01FF
	r, _, err := procDuplicateTokenEx.Call(
		uintptr(impToken), uintptr(tokenAllAccess), 0,
		uintptr(tokenImpersonation), uintptr(tokenPrimary), uintptr(unsafe.Pointer(&primary)),
	)
	if r == 0 {
		return 0, fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	return primary, nil
}

// LaunchInConsoleSession runs exePath with args as the currently logged-on
// console user, in their own interactive desktop session - the only way a
// service can make something appear on that user's screen.
func LaunchInConsoleSession(exePath string, args []string) error {
	token, err := primaryToken()
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(token)

	var envBlock uintptr
	r, _, _ := procCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&envBlock)), uintptr(token), 0)
	if r != 0 {
		defer procDestroyEnvironmentBlock.Call(envBlock)
	}

	cmdLine := syscall.EscapeArg(exePath)
	for _, a := range args {
		cmdLine += " " + syscall.EscapeArg(a)
	}
	cmdLineUTF16, err := syscall.UTF16PtrFromString(cmdLine)
	if err != nil {
		return err
	}
	desktop, err := syscall.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return err
	}

	si := startupInfo{lpDesktop: desktop}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi processInformation

	r, _, err = procCreateProcessAsUserW.Call(
		uintptr(token), 0, uintptr(unsafe.Pointer(cmdLineUTF16)), 0, 0, 0,
		uintptr(createUnicodeEnvironment|createNoWindow), envBlock, 0,
		uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)),
	)
	if r == 0 {
		return fmt.Errorf("CreateProcessAsUserW: %w", err)
	}
	syscall.CloseHandle(pi.hProcess)
	syscall.CloseHandle(pi.hThread)
	return nil
}

// ConsoleUserHomeDir resolves the profile directory (C:\Users\<name>) of
// whoever is currently logged into the console - what the service must
// back up instead of its own LocalSystem profile.
func ConsoleUserHomeDir() (string, error) {
	token, err := primaryToken()
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(token)

	buf := make([]uint16, 260)
	size := uint32(len(buf))
	r, _, err := procGetUserProfileDirectoryW.Call(
		uintptr(token), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return "", fmt.Errorf("GetUserProfileDirectoryW: %w", err)
	}
	return syscall.UTF16ToString(buf), nil
}
