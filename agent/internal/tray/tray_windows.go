//go:build windows

// Package tray implements the Windows notification-area (system tray)
// icon: last backup date on hover, and a small menu to trigger a backup or
// reschedule the next one. It runs as its own short-lived helper process
// (`backup-agent.exe --tray`), launched by the service into the console
// user's session (see winsession.LaunchInConsoleSession) since a service
// has no tray of its own to draw into. It talks to the service purely over
// the local control API on 127.0.0.1 - if the service is unreachable or
// this process misbehaves, the actual backup service is entirely
// unaffected, since it's a separate OS process.
//
// Pure Win32 via syscall, no CGO: RegisterClassExW/CreateWindowExW for a
// message-only window, Shell_NotifyIconW for the icon itself, and a
// classic CreatePopupMenu/TrackPopupMenu context menu - the standard,
// decades-old pattern for a Windows tray icon.
package tray

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"backup-agent/internal/config"
	"backup-agent/internal/osui"
)

// pidFilePath is where the running helper records its own PID, so a
// later launch or an uninstall can find and stop it precisely - reusing
// ServiceLogDir (already the one world-readable, service-owned location
// this agent uses on Windows) rather than inventing a new one.
func pidFilePath() (string, error) {
	dir, err := config.ServiceLogDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tray.pid"), nil
}

// KillRunningHelper stops a previously launched tray helper, if its PID
// file names one still alive. The helper is a standalone process in the
// console user's session (see LaunchInConsoleSession), not a child of the
// service - the service exiting or restarting never reaps it on its own,
// so every restart used to leave the previous icon running forever
// alongside a fresh one. Call this once before launching a new helper,
// and again on uninstall so the icon doesn't outlive the service it
// reports on. Best-effort throughout: a missing or stale PID file (the
// process already gone) is the common case, not an error.
func KillRunningHelper() {
	path, err := pidFilePath()
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
		_ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	}
	_ = os.Remove(path)
}

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procLoadIconW           = user32.NewProc("LoadIconW")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procShellNotifyIconW    = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmClose     = 0x0010
	wmDestroy   = 0x0002
	wmCommand   = 0x0111
	wmTrayIcon  = 0x8000 + 1 // WM_APP + 1
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	idiApplication = 32512

	mfString    = 0x00000000
	mfSeparator = 0x00000800
	mfGrayed    = 0x00000001
	mfDisabled  = 0x00000002

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	tpmNonNotify   = 0x0080

	menuBackupNow  = 1001
	menuReschedule = 1002
	menuOpenPanel  = 1003
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     syscall.Handle
	hIcon         syscall.Handle
	hCursor       syscall.Handle
	hbrBackground syscall.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       syscall.Handle
}

type point struct{ X, Y int32 }

type msgT struct {
	hwnd    syscall.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

// NOTIFYICONDATAW, Vista+ layout (we don't need the newest fields but the
// struct must be laid out correctly up to szTip/guidItem for Shell32 to
// read it safely regardless of the cbSize we declare).
type notifyIconDataW struct {
	cbSize           uint32
	hWnd             syscall.Handle
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            syscall.Handle
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uTimeout         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     syscall.Handle
}

func utf16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func setTip(dst *[128]uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	n := copy(dst[:], u)
	if n == len(dst) {
		dst[len(dst)-1] = 0
	}
}

type state struct {
	ControlBase string
}

// Run creates the tray icon and blocks in the Win32 message loop until the
// user quits it or the process is killed. controlBase is the service's
// local control API, e.g. "http://127.0.0.1:47812".
func Run(controlBase string) error {
	runtime.LockOSThread() // Win32 windows are bound to the thread that created them

	if path, err := pidFilePath(); err == nil {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
		defer os.Remove(path)
	}

	className := utf16("BackupAgentTrayWindowClass")
	hInstance, _, _ := procGetModuleHandleW.Call(0)

	var hwnd syscall.Handle
	var trayIcon notifyIconDataW
	httpClient := &http.Client{Timeout: 5 * time.Second}
	lastTip := ""

	wndProc := func(hWnd syscall.Handle, msg uint32, wParam, lParam uintptr) uintptr {
		switch msg {
		case wmTrayIcon:
			switch lParam {
			case wmLButtonUp, wmRButtonUp:
				showMenu(hWnd, controlBase, httpClient)
			}
			return 0
		case wmCommand:
			handleCommand(uint32(wParam&0xFFFF), controlBase, httpClient)
			return 0
		case wmClose:
			procDestroyWindow.Call(uintptr(hWnd))
			return 0
		case wmDestroy:
			trayIcon.uFlags = 0
			procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&trayIcon)))
			procPostQuitMessage.Call(0)
			return 0
		}
		r, _, _ := procDefWindowProcW.Call(uintptr(hWnd), uintptr(msg), wParam, lParam)
		return r
	}
	cb := syscall.NewCallback(wndProc)

	wc := wndClassExW{
		lpfnWndProc:   cb,
		hInstance:     syscall.Handle(hInstance),
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("RegisterClassExW: %w", err)
	}

	hwndRaw, _, err := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(utf16("Backup Agent"))),
		0, 0, 0, 0, 0, 0, 0, uintptr(hInstance), 0,
	)
	if hwndRaw == 0 {
		return fmt.Errorf("CreateWindowExW: %w", err)
	}
	hwnd = syscall.Handle(hwndRaw)

	icon, _, _ := procLoadIconW.Call(0, uintptr(idiApplication))

	trayIcon = notifyIconDataW{
		hWnd:             hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayIcon,
		hIcon:            syscall.Handle(icon),
	}
	trayIcon.cbSize = uint32(unsafe.Sizeof(trayIcon))
	setTip(&trayIcon.szTip, "Backup Agent")
	procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&trayIcon)))

	// Background refresh: keep the tooltip current without blocking the
	// message loop, which must stay responsive to clicks.
	go func() {
		const refreshInterval = 30 * time.Second
		// Ten misses in a row is five minutes with no reply at all - long
		// enough that a routine service restart never trips it, but a
		// service that's actually stopped or uninstalled does.
		const unreachableLimit = 10
		consecutiveFailures := 0
		for {
			st, err := fetchState(controlBase, httpClient)
			if err != nil {
				consecutiveFailures++
				if consecutiveFailures >= unreachableLimit {
					log.Print("tray: service injoignable depuis longtemps, fermeture de l'icône")
					procPostMessageW.Call(uintptr(hwnd), wmClose, 0, 0)
					return
				}
			} else {
				consecutiveFailures = 0
			}
			tip := "Backup Agent — état inconnu (service injoignable)"
			if err == nil {
				tip = "Backup Agent — " + summarize(st)
			}
			if tip != lastTip {
				lastTip = tip
				setTip(&trayIcon.szTip, tip)
				procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&trayIcon)))
			}
			time.Sleep(refreshInterval)
		}
	}()

	var m msgT
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	procDestroyWindow.Call(uintptr(hwnd))
	return nil
}

type trayState struct {
	DeviceName       string `json:"device_name"`
	LastBackupAt     string `json:"last_backup_at"`
	LastBackupStatus string `json:"last_backup_status"`
	Connected        bool   `json:"connected"`
}

func fetchState(base string, client *http.Client) (*trayState, error) {
	resp, err := client.Get(base + "/tray/state")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st trayState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func summarize(st *trayState) string {
	status := "jamais"
	if st.LastBackupAt != "" {
		if t, err := time.Parse(time.RFC3339, st.LastBackupAt); err == nil {
			status = t.Local().Format("02/01 15:04")
			if st.LastBackupStatus == "failed" {
				status += " (échec)"
			}
		}
	}
	conn := "hors ligne"
	if st.Connected {
		conn = "en ligne"
	}
	return "dernière sauvegarde " + status + " · " + conn
}

func showMenu(hwnd syscall.Handle, controlBase string, client *http.Client) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	label := "Dernière sauvegarde : chargement…"
	if st, err := fetchState(controlBase, client); err == nil {
		label = "Dernière sauvegarde : " + summarize(st)
	}
	procAppendMenuW.Call(menu, mfString|mfGrayed|mfDisabled, 0, uintptr(unsafe.Pointer(utf16(label))))
	procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	procAppendMenuW.Call(menu, mfString, menuBackupNow, uintptr(unsafe.Pointer(utf16("Sauvegarder maintenant"))))
	procAppendMenuW.Call(menu, mfString, menuReschedule, uintptr(unsafe.Pointer(utf16("Reprogrammer la prochaine sauvegarde…"))))
	procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	procAppendMenuW.Call(menu, mfString, menuOpenPanel, uintptr(unsafe.Pointer(utf16("Ouvrir le panneau"))))

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	// The SetForegroundWindow + trailing WM_NULL dance is Microsoft's own
	// documented requirement for a tray icon's popup menu to dismiss
	// itself properly when the user clicks away without selecting
	// anything - without it the menu can get stuck open.
	procSetForegroundWindow.Call(uintptr(hwnd))
	procTrackPopupMenu.Call(menu, tpmRightButton|tpmNonNotify, uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	procPostMessageW.Call(uintptr(hwnd), 0, 0, 0)

	// TrackPopupMenu without TPM_RETURNCMD posts WM_COMMAND to hwnd for us
	// when an item is actually picked; nothing further to do here.
}

func handleCommand(id uint32, controlBase string, client *http.Client) {
	switch id {
	case menuBackupNow:
		go func() {
			req, _ := http.NewRequest(http.MethodPost, controlBase+"/tray/backup-now", nil)
			if req != nil {
				_, _ = client.Do(req)
			}
		}()
	case menuReschedule:
		_ = osui.OpenBrowser(controlBase + "/tray/reschedule-page")
	case menuOpenPanel:
		_ = osui.OpenBrowser(controlBase + "/tray/open-panel")
	}
}
