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

	"golang.org/x/sys/windows"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	wtsapi32 = syscall.NewLazyDLL("wtsapi32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	userenv  = syscall.NewLazyDLL("userenv.dll")

	procWTSGetActiveConsoleSessionId = kernel32.NewProc("WTSGetActiveConsoleSessionId")
	procWTSQueryUserToken            = wtsapi32.NewProc("WTSQueryUserToken")
	procWTSEnumerateSessionsW        = wtsapi32.NewProc("WTSEnumerateSessionsW")
	procWTSFreeMemory                = wtsapi32.NewProc("WTSFreeMemory")
	procDuplicateTokenEx             = advapi32.NewProc("DuplicateTokenEx")
	procCreateEnvironmentBlock       = userenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock      = userenv.NewProc("DestroyEnvironmentBlock")
	procCreateProcessAsUserW         = advapi32.NewProc("CreateProcessAsUserW")
	procGetUserProfileDirectoryW     = userenv.NewProc("GetUserProfileDirectoryW")
)

// WTS_CONNECTSTATE_CLASS values we care about. A session in any of these
// states belongs to a real user with a loadable profile; the ones we skip
// (WTSConnectQuery, WTSShadow, WTSIdle, WTSListen, WTSDown, WTSInit) are
// either transient plumbing or not a user session at all.
const (
	wtsActive       = 0
	wtsConnected    = 1
	wtsDisconnected = 4
)

// wtsSessionInfo mirrors WTS_SESSION_INFOW. Field types (not hand-computed
// offsets) let Go apply the platform's own struct alignment, so this is
// correct on both 386 and amd64.
type wtsSessionInfo struct {
	SessionID       uint32
	pWinStationName *uint16
	State           uint32
}

// ConsoleUserSID returns the SID (as its canonical S-1-5-... string form)
// of whoever is logged into the console - used to read that user's own
// registry hive (HKEY_USERS\<SID>\...) from a LocalSystem service, since
// HKEY_CURRENT_USER inside the service process resolves to LocalSystem's
// own hive, not the console user's. Built on golang.org/x/sys/windows'
// own token/SID helpers rather than hand-rolled syscalls, since those
// already handle the LocalAlloc'd string memory correctly.
func ConsoleUserSID() (string, error) {
	token, err := primaryToken()
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(token)

	tokenUser, err := windows.Token(token).GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("GetTokenUser: %w", err)
	}
	sid := tokenUser.User.Sid
	if sid == nil {
		return "", fmt.Errorf("jeton sans SID utilisateur")
	}
	return sid.String(), nil
}

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

// tokenForSession asks for the user token of one session. Fails, normally
// and expectedly, for a session with no logged-on user (the login screen
// is itself a session).
func tokenForSession(id uint32) (syscall.Handle, error) {
	var userToken syscall.Handle
	r, _, err := procWTSQueryUserToken.Call(uintptr(id), uintptr(unsafe.Pointer(&userToken)))
	if r == 0 {
		return 0, fmt.Errorf("WTSQueryUserToken(session %d): %w", id, err)
	}
	return userToken, nil
}

// candidateSessionIDs lists the sessions worth trying for a user token,
// best first.
//
// The physical console comes first, but it is deliberately *not* the only
// candidate: WTSGetActiveConsoleSessionId only ever describes the machine's
// physically-attached session, and returns 0xFFFFFFFF outright when nobody
// is at it. Relying on it alone made this whole package - and so every
// folder path the agent resolves - fail on entirely ordinary machines:
// a user connected over RDP (their session is real and active but is never
// the console), a machine sitting at the lock or login screen when a
// backup is triggered from the panel, or a session in the middle of
// connecting. Enumerating the session table and accepting any session that
// actually yields a user token covers all of those.
func candidateSessionIDs() []uint32 {
	var ids []uint32
	seen := make(map[uint32]bool)
	add := func(id uint32) {
		// Session 0 is the isolated services session - it has no
		// interactive user, so it can never be the profile we want.
		if id == 0 || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}

	if consoleID, err := activeConsoleSessionID(); err == nil {
		add(consoleID)
	}

	var info *wtsSessionInfo
	var count uint32
	r, _, _ := procWTSEnumerateSessionsW.Call(
		0, // WTS_CURRENT_SERVER_HANDLE
		0, // Reserved
		1, // Version
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Pointer(&count)),
	)
	if r == 0 || info == nil {
		return ids
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(info)))

	sessions := unsafe.Slice(info, count)
	// Two passes so a fully-active session is always preferred over one
	// that's merely connected or disconnected, regardless of table order.
	for _, s := range sessions {
		if s.State == wtsActive {
			add(s.SessionID)
		}
	}
	for _, s := range sessions {
		if s.State == wtsConnected || s.State == wtsDisconnected {
			add(s.SessionID)
		}
	}
	return ids
}

// consoleUserToken returns a token for the best available logged-on user.
func consoleUserToken() (syscall.Handle, error) {
	ids := candidateSessionIDs()
	if len(ids) == 0 {
		return 0, fmt.Errorf("aucune session utilisateur sur ce poste (personne n'est connecté)")
	}
	var lastErr error
	for _, id := range ids {
		token, err := tokenForSession(id)
		if err == nil {
			return token, nil
		}
		lastErr = err
	}
	return 0, fmt.Errorf("aucune session utilisateur exploitable (%d essayée(s)): %w", len(ids), lastErr)
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
